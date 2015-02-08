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
	"errors"
	"flag"
	"fmt"
	"hash/fnv"
	"io"
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
	rules        []*rule
	config       *configuration
	logFile      *os.File
	matches      map[int64]*match
	matchesMu    = &sync.RWMutex{}
	errNoRules   = errors.New("no rules")
	errEmptyRule = errors.New("empty rule")
	configFile   = flag.String("config", ".gawp", "Configuration file")
	hasher64     = fnv.New64a()
	hasher64Mu   = &sync.Mutex{}
)

// Gawp configuration
type configuration struct {
	Recursive bool
	Workers   int
	Logfile   string
	Events    events
	Rules     map[string][]string
}

type rule struct {
	mu    *sync.Mutex
	match *regexp.Regexp
	cmds  []string
}

type match struct {
	rule *rule
	cmds [][]string
}

type events map[fsnotify.Op]struct{} // Emulate a "set"

// Checks if the event exists in the events set
func (e events) contains(op fsnotify.Op) bool {
	_, exists := e[op]

	return exists
}

// UnmarshalYAML unmarshals the string array
// into a event "set" for fast loopup
func (e *events) UnmarshalYAML(unmarshal func(interface{}) error) error {
	s := ""

	if err := unmarshal(&s); err != nil {
		return err
	} else if s == "" {
		return nil
	}

	*e = map[fsnotify.Op]struct{}{}

	for _, c := range strings.Split(s, ",") {
		switch strings.ToLower(strings.TrimSpace(c)) {
		case "create":
			(*e)[fsnotify.Create] = struct{}{}
		case "write":
			(*e)[fsnotify.Write] = struct{}{}
		case "rename":
			(*e)[fsnotify.Rename] = struct{}{}
		case "remove":
			(*e)[fsnotify.Remove] = struct{}{}
		case "chmod":
			(*e)[fsnotify.Chmod] = struct{}{}
		}
	}

	return nil
}

func main() {
	flag.Parse()
	log.SetFlags(log.Ldate | log.Lmicroseconds)

	dir, err := filepath.Abs(filepath.Dir(os.Args[0]))

	if err != nil {
		log.Fatal(err)
	}

	defer logFile.Close()

	if rules, err = load(dir, *configFile); err != nil {
		log.Fatalf("unable to load configuration file: %s (%s)", *configFile, err)
	}

	// Operating system threads that can execute user-level Go code simultaneously
	if config.Workers > 1 {
		n := runtime.NumCPU()

		if config.Workers < n {
			n = config.Workers
		}

		runtime.GOMAXPROCS(n)
	}

	log.Printf("loaded %d rules", len(rules))

	// File system notifications
	watcher, err := fsnotify.NewWatcher()

	if err != nil {
		log.Fatal(err)
	}

	defer watcher.Close()

	if config.Recursive {
		c := 0

		// Watch root and child directories
		filepath.Walk(dir+"/", func(path string, f os.FileInfo, err error) error {
			if !f.IsDir() {
				return nil
			}

			if n := path[len(dir):]; n != "/" && n[1] == '.' {
				return nil
			}

			if err := watcher.Add(path); err != nil {
				log.Printf("unable to watch path: %s (%s)", path, err)
			}

			c++

			return nil
		})

		log.Printf("watching %d directories", c)
	} else if err = watcher.Add(dir); err != nil {
		//  Only watch the root dir
		log.Fatal(err)
	}

	// Disable file system notifications for the log file
	if config.Logfile != "" {
		watcher.Remove(config.Logfile)
	}

	// We should always be able to run atleast 1 job
	if config.Workers < 1 {
		config.Workers = 1
	}

	var (
		filename string                                // Current filename path
		signals  = make(chan os.Signal, 2)             // OS signal capture
		throttle = make(chan struct{}, config.Workers) // Worker throttle
		wg       = &sync.WaitGroup{}
	)

	// Handle signals for clean shutdown
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)

	// Wait for workers on shutdown
	defer wg.Wait()

	for {
		select {
		case event := <-watcher.Events:
			if !config.Events.contains(event.Op) {
				continue
			}

			throttle <- struct{}{}

			// Reload config file
			if filename = event.Name[len(dir)+1:]; filename == *configFile {
				// Wait for active workers
				wg.Wait()

				log.Println("reloading config file")

				if rules, err = load(dir, filename); err != nil {
					log.Fatal(err)
				}

				log.Printf("loaded %d rules", len(rules))

				<-throttle

				continue
			}

			wg.Add(1)

			go worker(throttle, wg, filename)

		case err = <-watcher.Errors:
			log.Println("fsnotify error:", err)

		case <-signals:
			return
		}
	}
}

func worker(throttle chan struct{}, wg *sync.WaitGroup, f string) {
	defer func() {
		<-throttle

		wg.Done()
	}()

	m := findMatch(f)

	if m == nil {
		return
	}

	// Atomicity for the given rule
	m.rule.mu.Lock()

	defer m.rule.mu.Unlock()

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

// findMatch attempts to find a rule match for file path
// On success caches the match for fast future lookups
func findMatch(f string) *match {
	h := hash64(f)

	// Fast map lookup, circumvent regular expressions
	matchesMu.RLock()
	c, exists := matches[h]
	matchesMu.RUnlock()

	if exists {
		return c
	}

	var (
		s [][]string
		m *match
	)

	// Test each rule for a match
	for _, r := range rules {
		if s = r.match.FindAllStringSubmatch(f, -1); s == nil {
			continue
		}

		m = &match{rule: r}

		for _, cmd := range r.cmds {
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

	// Cache for fast lookup
	matchesMu.Lock()
	matches[h] = m
	matchesMu.Unlock()

	return m
}

// loads the Gawp config file and handles the loading of rules
func load(dir string, f string) ([]*rule, error) {
	h, err := os.Open(dir + "/" + f)

	if err != nil {
		return nil, err
	}

	defer h.Close()

	if config, err = loadConfig(h); err != nil {
		return nil, err
	}

	if err = setLogFile(dir); err != nil {
		return nil, err
	}

	return loadRules()
}

func loadConfig(in io.Reader) (*configuration, error) {
	b, err := ioutil.ReadAll(in)

	if err != nil {
		return nil, err
	}

	var c *configuration

	if err = yaml.Unmarshal(b, &c); err != nil {
		return nil, err
	}

	return c, nil
}

func loadRules() (rules []*rule, err error) {
	if len(config.Rules) == 0 {
		return nil, errNoRules
	}

	for k, v := range config.Rules {
		r := &rule{cmds: v, mu: &sync.Mutex{}}

		if r.match, err = regexp.Compile(k); err != nil {
			return nil, fmt.Errorf("rule compilation error: %s (%s)", k, err)
		}

		rules = append(rules, r)
	}

	// Init/reset matches cache
	matches = map[int64]*match{}

	return rules, nil
}

func setLogFile(dir string) (err error) {
	if config.Logfile == "" {
		return nil
	}

	// Relative path
	if config.Logfile[0] != '/' {
		config.Logfile = dir + "/" + config.Logfile
	}

	// Close current log file as location might have changed in the config
	// No side effects
	logFile.Close()

	if logFile, err = os.OpenFile(config.Logfile, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0666); err != nil {
		return err
	}

	log.SetOutput(logFile)

	return nil
}

// hash64 returns the hash of the given string as int64
func hash64(s string) int64 {
	hasher64Mu.Lock()

	defer hasher64Mu.Unlock()

	hasher64.Reset()
	hasher64.Write([]byte(s))

	return int64(hasher64.Sum64())
}
