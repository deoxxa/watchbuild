package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"math/rand"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/gobwas/glob"
	"github.com/segmentio/textio"
	"gopkg.in/yaml.v2"
)

type config struct {
	Ignore   []string   `yaml:"ignore"`
	Watchers []*watcher `yaml:"watchers"`

	m      sync.RWMutex
	ignore []glob.Glob
	ready  bool
}

func (c *config) setup() {
	c.m.RLock()
	if c.ready {
		c.m.RUnlock()
		return
	}
	c.m.RUnlock()

	c.m.Lock()
	defer c.m.Unlock()
	if c.ready {
		return
	}

	c.ignore = []glob.Glob{}
	for _, e := range c.Ignore {
		c.ignore = append(c.ignore, glob.MustCompile(e))
	}

	for _, w := range c.Watchers {
		w.setup()
	}

	c.ready = true
}

type watcher struct {
	Name            string        `yaml:"name"`
	Command         string        `yaml:"command"`
	Include         []string      `yaml:"include"`
	Exclude         []string      `yaml:"exclude"`
	Wait            time.Duration `yaml:"wait"`
	Debounce        time.Duration `yaml:"debounce"`
	HoldUntilChange bool          `yaml:"hold_until_change"`

	m       sync.RWMutex
	cmd     *exec.Cmd
	include []glob.Glob
	exclude []glob.Glob
	timer   *time.Timer
	ch      chan struct{}
	ready   bool
}

func (w *watcher) setup() {
	w.m.RLock()
	if w.ready {
		w.m.RUnlock()
		return
	}
	w.m.RUnlock()

	w.m.Lock()
	defer w.m.Unlock()
	if w.ready {
		return
	}

	if w.Debounce == 0 {
		w.Debounce = time.Millisecond * 500
	}

	for _, e := range w.Include {
		w.include = append(w.include, glob.MustCompile(e, '/'))
	}
	for _, e := range w.Exclude {
		w.exclude = append(w.exclude, glob.MustCompile(e, '/'))
	}

	w.ch = make(chan struct{}, 1)
	if !w.HoldUntilChange {
		w.ch <- struct{}{}
	}

	w.ready = true
}

func (w *watcher) match(s string) bool {
	w.setup()

	for _, e := range w.exclude {
		if e.Match(s) {
			return false
		}
	}

	for _, e := range w.include {
		if e.Match(s) {
			return true
		}
	}

	return len(w.include) == 0
}

func (w *watcher) touch(s string) {
	w.setup()

	if !w.match(s) {
		return
	}

	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}

	w.timer = time.AfterFunc(w.Debounce, func() {
		w.timer = nil
		w.ch <- struct{}{}
	})
}

func (w *watcher) run() {
	w.setup()

	for range w.ch {
		fmt.Printf("RUN %s %q\n", w.Name, w.Command)

		if w.cmd != nil {
			pgid, err := syscall.Getpgid(w.cmd.Process.Pid)
			if err != nil {
				fmt.Printf("[%s] couldn't get pgid for process %d: %s\n", w.Name, w.cmd.Process.Pid, err.Error())
			}

			done := make(chan error, 1)

			go func() { done <- w.cmd.Wait() }()

			if err := syscall.Kill(-pgid, syscall.SIGTERM); err != nil {
				fmt.Printf("[%s] couldn't send SIGTERM to process %d: %s\n", w.Name, w.cmd.Process.Pid, err.Error())
			}

			select {
			case <-time.After(3 * time.Second):
				if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil {
					fmt.Printf("[%s] couldn't send SIGKILL to process %d: %s\n", w.Name, w.cmd.Process.Pid, err.Error())
				}
			case err := <-done:
				if err != nil {
					fmt.Printf("[%s] process finished with error: %s\n", w.Name, err.Error())
				} else {
					fmt.Printf("[%s] process finished successfully\n", w.Name)
				}
			}
		}

		w.cmd = exec.Command("sh", []string{"-c", w.Command}...)
		w.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		w.cmd.Env = append([]string{}, os.Environ()...)
		w.cmd.Stdout = textio.NewPrefixWriter(os.Stdout, "["+w.Name+"] ")
		w.cmd.Stderr = textio.NewPrefixWriter(os.Stderr, "["+w.Name+"] ")
		w.cmd.Start()
	}
}

var (
	verbose = flag.Bool("verbose", false, "Log more information")
)

func main() {
	flag.Parse()

	rand.Seed(time.Now().UnixNano())

	d, err := ioutil.ReadFile("reloader.yml")
	if err != nil {
		panic(err)
	}

	var c config
	if err := yaml.Unmarshal(d, &c); err != nil {
		panic(err)
	}

	c.setup()

	for _, w := range c.Watchers {
		go w.run()
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		panic(err)
	}
	defer watcher.Close()

	dirs, err := findAllDirectories(".")
	if err != nil {
		panic(err)
	}

outer:
	for _, dir := range dirs {
		for _, e := range c.ignore {
			if e.Match(dir) {
				continue outer
			}
		}

		if *verbose {
			fmt.Printf("ADD %s\n", dir)
		}

		if err := watcher.Add(dir); err != nil {
			panic(err)
		}
	}

	defer func() {
		for _, w := range c.Watchers {
			if w.cmd != nil {
				syscall.Kill(-w.cmd.Process.Pid, syscall.SIGKILL)
				w.cmd.Wait()
			}
		}
	}()

	ch := make(chan os.Signal, 2)
	signal.Notify(ch, os.Interrupt, os.Kill)
	go func() {
		<-ch
		for _, w := range c.Watchers {
			if w.cmd != nil && w.cmd.Process != nil {
				syscall.Kill(-w.cmd.Process.Pid, syscall.SIGKILL)
			}
		}
		os.Exit(1)
	}()

	for ev := range watcher.Events {
		name := strings.TrimLeft(ev.Name, "./")
		if *verbose {
			fmt.Printf("%s %s\n", ev.Op, name)
		}
		for i := range c.Watchers {
			w := c.Watchers[i]
			w.touch(name)
		}
	}
}

func findAllDirectories(dir string) ([]string, error) {
	l := []string{dir}

	a, err := ioutil.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	for _, e := range a {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}

		s := filepath.Join(dir, e.Name())

		d, err := findAllDirectories(s)
		if err != nil {
			return nil, err
		}

		l = append(l, d...)
	}

	return l, nil
}
