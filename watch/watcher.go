// Copyright 2014 The lime Authors.
// Use of this source code is governed by a 2-clause
// BSD-style license that can be found in the LICENSE file.

package watch

import (
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/limetext/backend/log"
	"github.com/limetext/util"
	"gopkg.in/fsnotify.v1"
)

type (
	// Wrapper around fsnotify watcher to suit lime needs
	// 	- Watching directories, we will have less individual watchers
	// 	- Have multiple subscribers on single file or directory
	// 	- Watching a path which doesn't exist yet
	// 	- Watching and applying action on certain events
	Watcher struct {
		sync.Mutex
		wchr     *fsnotify.Watcher
		watched  map[string][]interface{}
		watchers []string // paths we created watcher on
		dirs     []string // dirs we are watching
	}

	// Called on file change directories won't recieve this callback
	FileChangedCallback interface {
		FileChanged(string)
	}
	// Called on a file or directory is created
	FileCreatedCallback interface {
		FileCreated(string)
	}
	// Called on a file or directory is removed
	FileRemovedCallback interface {
		FileRemoved(string)
	}
	// Called when a directory or file is renamed
	// TODO: fsnotify behavior after rename is obscure
	// if we have foo dir with a bar file inside and we are watching foo dir,
	// on renaming foo to boo we will get rename event for foo dir but if we
	// delete boo/bar we will get remove event for foo/bar not boo/bar
	FileRenamedCallback interface {
		FileRenamed(string)
	}
)

func NewWatcher() (*Watcher, error) {
	wchr, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	w := &Watcher{wchr: wchr}
	w.watched = make(map[string][]interface{})
	w.watchers = make([]string, 0)
	w.dirs = make([]string, 0)

	return w, nil
}

func (w *Watcher) Watch(name string, cb interface{}) error {
	log.Finest("Watch(%s)", name)
	fi, err := os.Stat(name)
	isDir := err == nil && fi.IsDir()
	// If the file doesn't exist currently we will add watcher for file
	// directory and look for create event inside the directory
	if os.IsNotExist(err) {
		log.Fine("%s doesn't exist, Watching parent directory", name)
		if err := w.Watch(filepath.Dir(name), nil); err != nil {
			return err
		}
	}
	w.Lock()
	defer w.Unlock()
	if err := w.add(name, cb); err != nil {
		if !isDir {
			return err
		}
		if util.Exists(w.dirs, name) {
			log.Fine("%s is watched already", name)
			return nil
		}
	}
	// If exists in watchers we are already watching the path
	// Or
	// If the file is under one of watched dirs
	//
	// no need to create watcher
	if util.Exists(w.watchers, name) || (!isDir && util.Exists(w.dirs, filepath.Dir(name))) {
		return nil
	}
	if err := w.watch(name); err != nil {
		return err
	}
	if isDir {
		w.flushDir(name)
	}
	return nil
}

func (w *Watcher) add(name string, cb interface{}) error {
	numok := 0
	if _, ok := cb.(FileChangedCallback); ok {
		numok++
	}
	if _, ok := cb.(FileCreatedCallback); ok {
		numok++
	}
	if _, ok := cb.(FileRemovedCallback); ok {
		numok++
	}
	if _, ok := cb.(FileRenamedCallback); ok {
		numok++
	}
	if numok == 0 {
		return errors.New("The callback argument does satisfy any File*Callback interfaces")
	}
	w.watched[name] = append(w.watched[name], cb)
	return nil
}

func (w *Watcher) watch(name string) error {
	if err := w.wchr.Add(name); err != nil {
		return err
	}
	w.watchers = append(w.watchers, name)
	return nil
}

// Remove watchers created on files under this directory because
// one watcher on the parent directory is enough for all of them
func (w *Watcher) flushDir(name string) {
	if util.Exists(w.dirs, name) {
		return
	}
	w.dirs = append(w.dirs, name)
	for _, p := range w.watchers {
		if filepath.Dir(p) == name && !util.Exists(w.dirs, p) {
			if err := w.removeWatch(p); err != nil {
				log.Error("Couldn't unwatch file %s: %s", p, err)
			}
		}
	}
}

func (w *Watcher) UnWatch(name string, cb interface{}) error {
	log.Finest("UnWatch(%s)", name)
	w.Lock()
	defer w.Unlock()
	if cb == nil {
		return w.unWatch(name)
	}
	for i, c := range w.watched[name] {
		if c == cb {
			w.watched[name][i] = w.watched[name][len(w.watched[name])-1]
			w.watched[name][len(w.watched[name])-1] = nil
			w.watched[name] = w.watched[name][:len(w.watched[name])-1]
			break
		}
	}
	if len(w.watched[name]) == 0 {
		w.unWatch(name)
	}
	return nil
}

func (w *Watcher) unWatch(name string) error {
	delete(w.watched, name)
	if err := w.removeWatch(name); err != nil {
		return err
	}
	return nil
}

func (w *Watcher) removeWatch(name string) error {
	if err := w.wchr.Remove(name); err != nil {
		return err
	}
	w.watchers = util.Remove(w.watchers, name)
	if util.Exists(w.dirs, name) {
		w.removeDir(name)
	}
	return nil
}

// Put back watchers on watching files under the directory
func (w *Watcher) removeDir(name string) {
	for p, _ := range w.watched {
		if filepath.Dir(p) == name {
			if err := w.watch(p); err != nil {
				log.Error("Could not watch: %s", err)
				continue
			}
		}
	}
	w.dirs = util.Remove(w.dirs, name)
}

// Observe dispatches notifications received by the watcher. This function will
// return when the watcher is closed.
func (w *Watcher) Observe() {
	for {
		select {
		case ev, ok := <-w.wchr.Events:
			if !ok {
				break
			}
			func() {
				w.Lock()
				defer w.Unlock()
				w.apply(ev)
				name := ev.Name
				// currently fsnotify pushs remove event for files
				// inside directory when a directory is removed but
				// when the directory is renamed there is no event for
				// files inside directory
				if ev.Op&fsnotify.Rename != 0 && util.Exists(w.dirs, name) {
					for p, _ := range w.watched {
						if filepath.Dir(p) == name {
							ev.Name = p
							w.apply(ev)
						}
					}
				}
				dir := filepath.Dir(name)
				// The watcher will be removed if the file is deleted
				// so we need to watch the parent directory for when the
				// file is created again
				if ev.Op&fsnotify.Remove != 0 {
					w.watchers = util.Remove(w.watchers, name)
					w.Unlock()
					w.Watch(dir, nil)
					w.Lock()
				}
				// If the event is create we will apply FileCreated callback
				// for the parent directory to because when new file is created
				// inside directory we won't get any event for the watched directory.
				// we need this feature to detect new packages(plugins, settings, etc)
				if cbs, exist := w.watched[dir]; ev.Op&fsnotify.Create != 0 && exist {
					for _, cb := range cbs {
						if c, ok := cb.(FileCreatedCallback); ok {
							w.Unlock()
							c.FileCreated(name)
							w.Lock()
						}
					}
				}

			}()
		case err, ok := <-w.wchr.Errors:
			if !ok {
				break
			}
			log.Warn("Watcher error: %s", err)
		}
	}
}

func (w *Watcher) apply(ev fsnotify.Event) {
	for _, cb := range w.watched[ev.Name] {
		if ev.Op&fsnotify.Create != 0 {
			if c, ok := cb.(FileCreatedCallback); ok {
				c.FileCreated(ev.Name)
			}
		}
		if ev.Op&fsnotify.Write != 0 {
			if c, ok := cb.(FileChangedCallback); ok {
				c.FileChanged(ev.Name)
			}
		}
		if ev.Op&fsnotify.Remove != 0 {
			if c, ok := cb.(FileRemovedCallback); ok {
				c.FileRemoved(ev.Name)
			}
		}
		if ev.Op&fsnotify.Rename != 0 {
			if c, ok := cb.(FileRenamedCallback); ok {
				c.FileRenamed(ev.Name)
			}
		}
	}
}
