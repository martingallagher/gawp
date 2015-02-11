// Copyright Praegressus Limited. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// +build !plan9,!solaris

package main

import (
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"hash/fnv"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"gopkg.in/fsnotify.v1"
	"gopkg.in/yaml.v2"
)

var (
	config     *configuration
	logFile    *os.File
	watcher    *fsnotify.Watcher
	rules      map[fsnotify.Op][]*rule
	rulesMu    = &sync.RWMutex{}
	matches    map[uint64]*match
	matchesMu  = &sync.RWMutex{}
	errNoRules = errors.New("no rules")
	configFile = flag.String("config", ".gawp", "Configuration file")
	hasher64   = fnv.New64a()
	hasher64Mu = &sync.Mutex{}
)

// Gawp configuration
type configuration struct {
	recursive, verbose bool
	workers            int
	logFile            string
}

type rule struct {
	match *regexp.Regexp
	cmds  []string
}

type match struct {
	mu   *sync.Mutex
	rule *rule
	cmds [][]string
}

func main() {
	flag.Parse()
	log.SetFlags(log.Ldate | log.Lmicroseconds)

	dir, err := filepath.Abs(filepath.Dir(os.Args[0]))

	if err != nil {
		log.Fatal(err)
	}

	defer logFile.Close()

	if err = load(dir, *configFile); err != nil {
		log.Fatalf("unable to load configuration file: %s (%s)", *configFile, err)
	}

	// Operating system threads that can execute user-level Go code simultaneously
	if config.workers > 1 {
		n := runtime.NumCPU()

		if config.workers < n {
			n = config.workers
		}

		runtime.GOMAXPROCS(n)
	} else if config.workers < 1 {
		// Atleast 1 worker needed
		config.workers = 1
	}

	// File system notifications
	if watcher, err = fsnotify.NewWatcher(); err != nil {
		log.Fatal(err)
	}

	defer watcher.Close()

	if config.recursive {
		// Watch root and child directories
		if err = filepath.Walk(dir+"/", walk); err != nil {
			log.Fatal(err)
		}
	} else if err = watcher.Add(dir); err != nil {
		//  Only watch the root dir
		log.Fatal(err)
	}

	log.Println("started Gawp")

	// Disable file system notifications for the log file
	if config.logFile != "" {
		if err = watcher.Remove(config.logFile); err != nil {
			log.Println(err)
		}
	}

	var (
		signals  = make(chan os.Signal, 2)             // OS signal capture
		throttle = make(chan struct{}, config.workers) // Worker throttle
		wg       = &sync.WaitGroup{}
	)

	// Handle signals for clean shutdown
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)

	// Wait for workers on shutdown
	defer wg.Wait()

	for {
		select {
		case event := <-watcher.Events:
			filename := event.Name[len(dir)+1:]

			if config.verbose {
				log.Println(event.String())
			}

			throttle <- struct{}{}

			// Reload config file
			if filename == *configFile {
				// Wait for active workers
				wg.Wait()
				log.Println("reloading config file")

				l := config.logFile

				if err = load(dir, filename); err != nil {
					log.Fatal(err)
				}

				if config.logFile != "" && config.logFile != l {
					if err = watcher.Remove(config.logFile); err != nil {
						log.Println(err)
					}
				}

				<-throttle

				continue
			}

			// Add or stop watching directories
			if err = handleEvent(event); err != nil {
				log.Println(err)

				continue
			}

			wg.Add(1)

			go worker(throttle, wg, event.Op, filename)

		case err = <-watcher.Errors:
			log.Println("fsnotify error:", err)

		case <-signals:
			return
		}
	}
}

func worker(throttle chan struct{}, wg *sync.WaitGroup, e fsnotify.Op, f string) {
	defer func() {
		<-throttle

		wg.Done()
	}()

	m := findMatch(e, f)

	if m == nil {
		return
	}

	// Atomicity for the given match
	m.mu.Lock()

	defer m.mu.Unlock()

	var cmd *exec.Cmd

	for i, c := range m.cmds {
		if len(c) == 1 {
			cmd = exec.Command(c[0])
		} else {
			cmd = exec.Command(c[0], c[1:]...)
		}

		if b, err := cmd.Output(); err != nil {
			log.Printf("command (%s) error: %s", c, err)
		} else if len(b) > 0 {
			log.Printf("%s\n%s", m.rule.cmds[i], b)
		}
	}
}

// walk implements filepath.WalkFunc; adding each directory
// to the file system notifications watcher
func walk(path string, f os.FileInfo, err error) error {
	if !f.IsDir() || f.Name()[0] == '.' {
		return nil
	}

	if err := watcher.Add(path); err != nil {
		log.Printf("unable to watch path: %s (%s)", path, err)
	}

	return nil
}

// handleEvent determines the nature of the event, adding
// or removing directories to the file system notifications watcher
func handleEvent(e fsnotify.Event) error {
	create := e.Op&fsnotify.Create == fsnotify.Create

	if !create && e.Op&fsnotify.Remove != fsnotify.Remove {
		return nil
	}

	h, err := os.Open(e.Name)

	if err != nil {
		return err
	}

	defer h.Close()

	s, err := h.Stat()

	if err != nil {
		return err
	} else if !s.IsDir() {
		return nil
	}

	if create {
		if config.recursive {
			return filepath.Walk(e.Name+"/", walk)
		}

		return watcher.Add(e.Name)
	}

	return watcher.Remove(e.Name)
}

// findMatch attempts to find a rule match for file path
// On success caches the match for fast future lookups
func findMatch(e fsnotify.Op, f string) *match {
	var (
		m *match
		h = hash64(e, f)
	)

	// Fast map lookup, circumvent regular expressions
	matchesMu.RLock()
	c, exists := matches[h]
	matchesMu.RUnlock()

	if exists {
		return c
	}

	// Cache for fast lookup
	defer func() {
		matchesMu.Lock()
		matches[h] = m
		matchesMu.Unlock()
	}()

	rulesMu.RLock()

	defer rulesMu.RUnlock()

	// Check there's rules associated with the event type
	r, exists := rules[e]

	if !exists {
		return nil
	}

	// Test each rule for a match
	for _, c := range r {
		s := c.match.FindAllStringSubmatch(f, -1)

		if s == nil {
			continue
		}

		m = &match{&sync.Mutex{}, c, nil}

		for _, cmd := range c.cmds {
			for i := range s[0] {
				if i == 0 {
					continue
				}

				cmd = strings.Replace(cmd, "$"+strconv.Itoa(i), s[0][i], -1)
			}

			m.cmds = append(m.cmds, strings.Fields(strings.Replace(cmd, "$file", f, -1)))
		}

		break
	}

	return m
}

// load loads the Gawp config file and handles the loading of rules
func load(dir, f string) error {
	// Init/reset config, rules and matches cache
	config = &configuration{}
	rules = map[fsnotify.Op][]*rule{}
	matches = map[uint64]*match{}

	// Open config file
	h, err := os.Open(dir + "/" + f)

	if err != nil {
		return err
	}

	defer h.Close()

	b, err := ioutil.ReadAll(h)

	if err != nil {
		return err
	}

	var c map[string]interface{}

	if err = yaml.Unmarshal(b, &c); err != nil {
		return err
	}

	if len(c) == 0 {
		return errNoRules
	}

	for k, v := range c {
		switch k {
		case "recursive":
			config.recursive, _ = v.(bool)

		case "verbose":
			config.verbose, _ = v.(bool)

		case "workers":
			config.workers, _ = v.(int)

		case "logfile":
			config.logFile, _ = v.(string)

		default:
			if err = parseRules(k, v); err != nil {
				return err
			}
		}
	}

	if config.workers < 1 {
		config.workers = 1
	}

	if err = setLogFile(dir, config.logFile); err != nil {
		return err
	}

	return nil
}

// parseRules builds the rules map, adding rules into
// its defined event "bucket"
func parseRules(s string, i interface{}) error {
	e := parseEvents(s)

	if len(e) == 0 {
		return nil
	}

	var err error

	switch c := i.(type) {
	case map[interface{}]interface{}:
		for k, v := range c {
			// Regular expression
			m, ok := k.(string)

			if !ok {
				return nil
			}

			// Commands
			p, ok := v.([]interface{})

			if !ok || len(p) == 0 {
				return nil
			}

			r := &rule{}

			if r.match, err = regexp.Compile(m); err != nil {
				return fmt.Errorf("rule compilation error: %s (%s)", m, err)
			}

			for _, c := range p {
				cmd, ok := c.(string)

				if !ok || cmd == "" {
					continue
				}

				if cmd = strings.TrimSpace(cmd); cmd == "" {
					continue
				}

				r.cmds = append(r.cmds, cmd)
			}

			if len(r.cmds) == 0 {
				continue
			}

			// Add the rule to each event bucket
			for _, c := range e {
				rules[c] = append(rules[c], r)
			}
		}
	}

	return nil
}

// parseEvents returns the fsnotify.Op values
// for the events in the string
func parseEvents(s string) (e []fsnotify.Op) {
	for _, v := range strings.Split(s, ",") {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "create":
			e = append(e, fsnotify.Create)
		case "write":
			e = append(e, fsnotify.Write)
		case "rename":
			e = append(e, fsnotify.Rename)
		case "remove":
			e = append(e, fsnotify.Remove)
		case "chmod":
			e = append(e, fsnotify.Chmod)
		}
	}

	return
}

// setLogFile sets the logger output destination
func setLogFile(dir, f string) error {
	if f == "" {
		log.SetOutput(os.Stdout)

		return nil
	}

	// Relative path
	if f[0] != '/' {
		f = dir + "/" + f
	}

	// Force log file rotation, no side effects
	logFile.Close()
	logFile, err := os.OpenFile(f, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0666)

	if err != nil {
		return err
	}

	log.SetOutput(logFile)

	return nil
}

// hash64 returns the hash of the given FS operation & string as uint64
func hash64(e fsnotify.Op, s string) uint64 {
	hasher64Mu.Lock()

	defer hasher64Mu.Unlock()

	hasher64.Reset()
	binary.Write(hasher64, binary.LittleEndian, e)
	hasher64.Write([]byte(s))

	return hasher64.Sum64()
}
