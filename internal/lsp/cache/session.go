// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package cache

import (
	"context"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"golang.org/x/tools/internal/lsp/debug"
	"golang.org/x/tools/internal/lsp/source"
	"golang.org/x/tools/internal/lsp/telemetry"
	"golang.org/x/tools/internal/span"
	"golang.org/x/tools/internal/telemetry/log"
	"golang.org/x/tools/internal/telemetry/trace"
	"golang.org/x/tools/internal/xcontext"
	errors "golang.org/x/xerrors"
)

type session struct {
	cache *cache
	id    string

	options source.Options

	viewMu  sync.Mutex
	views   []*view
	viewMap map[span.URI]*view

	overlayMu sync.Mutex
	overlays  map[span.URI]*overlay

	openFiles     sync.Map
	filesWatchMap *WatchMap
}

type overlay struct {
	session *session
	uri     span.URI
	data    []byte
	hash    string
	version float64
	kind    source.FileKind

	// sameContentOnDisk is true if a file has been saved on disk,
	// and therefore does not need to be part of the overlay sent to go/packages.
	sameContentOnDisk bool
}

func (s *session) Options() source.Options {
	return s.options
}

func (s *session) SetOptions(options source.Options) {
	s.options = options
}

func (s *session) Shutdown(ctx context.Context) {
	s.viewMu.Lock()
	defer s.viewMu.Unlock()
	for _, view := range s.views {
		view.shutdown(ctx)
	}
	s.views = nil
	s.viewMap = nil
	debug.DropSession(debugSession{s})
}

func (s *session) Cache() source.Cache {
	return s.cache
}

func (s *session) NewView(ctx context.Context, name string, folder span.URI, options source.Options) (source.View, source.Snapshot, error) {
	s.viewMu.Lock()
	defer s.viewMu.Unlock()
	v, snapshot, err := s.createView(ctx, name, folder, options)
	if err != nil {
		return nil, nil, err
	}
	s.views = append(s.views, v)
	// we always need to drop the view map
	s.viewMap = make(map[span.URI]*view)
	return v, snapshot, nil
}

func (s *session) createView(ctx context.Context, name string, folder span.URI, options source.Options) (*view, *snapshot, error) {
	index := atomic.AddInt64(&viewIndex, 1)
	// We want a true background context and not a detached context here
	// the spans need to be unrelated and no tag values should pollute it.
	baseCtx := trace.Detach(xcontext.Detach(ctx))
	backgroundCtx, cancel := context.WithCancel(baseCtx)
	v := &view{
		session:       s,
		id:            strconv.FormatInt(index, 10),
		options:       options,
		baseCtx:       baseCtx,
		backgroundCtx: backgroundCtx,
		cancel:        cancel,
		name:          name,
		folder:        folder,
		filesByURI:    make(map[span.URI]viewFile),
		filesByBase:   make(map[string][]viewFile),
		snapshot: &snapshot{
			packages:          make(map[packageKey]*packageHandle),
			ids:               make(map[span.URI][]packageID),
			metadata:          make(map[packageID]*metadata),
			files:             make(map[span.URI]source.FileHandle),
			importedBy:        make(map[packageID][]packageID),
			actions:           make(map[actionKey]*actionHandle),
			workspacePackages: make(map[packageID]bool),
		},
		ignoredURIs: make(map[span.URI]struct{}),
		builtin:     &builtinPkg{},
	}
	v.snapshot.view = v

	if v.session.cache.options != nil {
		v.session.cache.options(&v.options)
	}

	// Preemptively build the builtin package,
	// so we immediately add builtin.go to the list of ignored files.
	v.buildBuiltinPackage(ctx)

	// Preemptively load everything in this directory.
	// TODO(matloob): Determine if this can be done in parallel with something else.
	// Perhaps different calls to NewView can be run in parallel?
	v.snapshotMu.Lock()
	defer v.snapshotMu.Unlock() // The code after the snapshot is used isn't expensive.
	m, err := v.snapshot.load(ctx, source.DirectoryURI(folder))
	if err != nil {
		// Suppress all errors.
		log.Error(ctx, "failed to load snapshot", err, telemetry.Directory.Of(folder))
		return v, v.snapshot, nil
	}
	// Prepare CheckPackageHandles for every package that's been loaded.
	// (*snapshot).CheckPackageHandle makes the assumption that every package that's
	// been loaded has an existing checkPackageHandle.
	if _, err := v.snapshot.checkWorkspacePackages(ctx, m); err != nil {
		// Suppress all errors.
		log.Error(ctx, "failed to check snapshot", err, telemetry.Directory.Of(folder))
		return v, v.snapshot, nil
	}

	debug.AddView(debugView{v})
	return v, v.snapshot, nil
}

// View returns the view by name.
func (s *session) View(name string) source.View {
	s.viewMu.Lock()
	defer s.viewMu.Unlock()
	for _, view := range s.views {
		if view.Name() == name {
			return view
		}
	}
	return nil
}

// ViewOf returns a view corresponding to the given URI.
// If the file is not already associated with a view, pick one using some heuristics.
func (s *session) ViewOf(uri span.URI) (source.View, error) {
	return s.viewOf(uri)
}

func (s *session) viewOf(uri span.URI) (*view, error) {
	s.viewMu.Lock()
	defer s.viewMu.Unlock()

	// Check if we already know this file.
	if v, found := s.viewMap[uri]; found {
		return v, nil
	}
	// Pick the best view for this file and memoize the result.
	v, err := s.bestView(uri)
	if err != nil {
		return nil, err
	}
	s.viewMap[uri] = v
	return v, nil
}

func (s *session) viewsOf(uri span.URI) []*view {
	s.viewMu.Lock()
	defer s.viewMu.Unlock()

	var views []*view
	for _, view := range s.views {
		if strings.HasPrefix(string(uri), string(view.Folder())) {
			views = append(views, view)
		}
	}
	return views
}

func (s *session) Views() []source.View {
	s.viewMu.Lock()
	defer s.viewMu.Unlock()
	result := make([]source.View, len(s.views))
	for i, v := range s.views {
		result[i] = v
	}
	return result
}

// bestView finds the best view to associate a given URI with.
// viewMu must be held when calling this method.
func (s *session) bestView(uri span.URI) (*view, error) {
	if len(s.views) == 0 {
		return nil, errors.Errorf("no views in the session")
	}
	// we need to find the best view for this file
	var longest *view
	for _, view := range s.views {
		if longest != nil && len(longest.Folder()) > len(view.Folder()) {
			continue
		}
		if strings.HasPrefix(string(uri), string(view.Folder())) {
			longest = view
		}
	}
	if longest != nil {
		return longest, nil
	}
	// TODO: are there any more heuristics we can use?
	return s.views[0], nil
}

func (s *session) removeView(ctx context.Context, view *view) error {
	s.viewMu.Lock()
	defer s.viewMu.Unlock()
	i, err := s.dropView(ctx, view)
	if err != nil {
		return err
	}
	// delete this view... we don't care about order but we do want to make
	// sure we can garbage collect the view
	s.views[i] = s.views[len(s.views)-1]
	s.views[len(s.views)-1] = nil
	s.views = s.views[:len(s.views)-1]
	return nil
}

func (s *session) updateView(ctx context.Context, view *view, options source.Options) (*view, *snapshot, error) {
	s.viewMu.Lock()
	defer s.viewMu.Unlock()
	i, err := s.dropView(ctx, view)
	if err != nil {
		return nil, nil, err
	}
	v, snapshot, err := s.createView(ctx, view.name, view.folder, options)
	if err != nil {
		// we have dropped the old view, but could not create the new one
		// this should not happen and is very bad, but we still need to clean
		// up the view array if it happens
		s.views[i] = s.views[len(s.views)-1]
		s.views[len(s.views)-1] = nil
		s.views = s.views[:len(s.views)-1]
	}
	// substitute the new view into the array where the old view was
	s.views[i] = v
	return v, snapshot, nil
}

func (s *session) dropView(ctx context.Context, v *view) (int, error) {
	// we always need to drop the view map
	s.viewMap = make(map[span.URI]*view)
	for i := range s.views {
		if v == s.views[i] {
			// we found the view, drop it and return the index it was found at
			s.views[i] = nil
			v.shutdown(ctx)
			return i, nil
		}
	}
	return -1, errors.Errorf("view %s for %v not found", v.Name(), v.Folder())
}

func (s *session) DidModifyFile(ctx context.Context, c source.FileModification) error {
	ctx = telemetry.URI.With(ctx, c.URI)
	for _, view := range s.viewsOf(c.URI) {
		switch c.Action {
		case source.Open:
			kind := source.DetectLanguage(c.LanguageID, c.URI.Filename())
			if kind == source.UnknownKind {
				return errors.Errorf("DidModifyFile: unknown file kind for %s", c.URI)
			}
			return s.didOpen(ctx, c.URI, kind, c.Version, c.Text)
		case source.Save:
			s.didSave(c.URI, c.Version)
		case source.Close:
			s.didClose(c.URI)
		}

		// Set the content for the file, only for didChange and didClose events.
		switch c.Action {
		case source.Change, source.Close:
			f, err := view.GetFile(ctx, c.URI)
			if err != nil {
				return err
			}
			view.setContent(ctx, f, c.Version, c.Text)
		}
	}
	return nil
}

// TODO: Propagate the language ID through to the view.
func (s *session) didOpen(ctx context.Context, uri span.URI, kind source.FileKind, version float64, text []byte) error {
	ctx = telemetry.File.With(ctx, uri)

	// Files with _ prefixes are ignored.
	if strings.HasPrefix(filepath.Base(uri.Filename()), "_") {
		for _, view := range s.views {
			view.ignoredURIsMu.Lock()
			view.ignoredURIs[uri] = struct{}{}
			view.ignoredURIsMu.Unlock()
		}
		return nil
	}

	// Make sure that the file gets added to the session's file watch map.
	view, err := s.bestView(uri)
	if err != nil {
		return err
	}
	if _, err := view.GetFile(ctx, uri); err != nil {
		return err
	}

	// Mark the file as open.
	s.openFiles.Store(uri, true)

	// Read the file on disk and compare it to the text provided.
	// If it is the same as on disk, we can avoid sending it as an overlay to go/packages.
	s.openOverlay(ctx, uri, kind, version, text)
	return nil
}

func (s *session) didSave(uri span.URI, version float64) {
	s.overlayMu.Lock()
	defer s.overlayMu.Unlock()

	if overlay, ok := s.overlays[uri]; ok {
		overlay.sameContentOnDisk = true
		overlay.version = version
	}
}

func (s *session) didClose(uri span.URI) {
	s.openFiles.Delete(uri)
}

func (s *session) IsOpen(uri span.URI) bool {
	_, open := s.openFiles.Load(uri)
	return open
}

func (s *session) GetFile(uri span.URI, kind source.FileKind) source.FileHandle {
	if overlay := s.readOverlay(uri); overlay != nil {
		return overlay
	}
	// Fall back to the cache-level file system.
	return s.cache.GetFile(uri, kind)
}

func (s *session) setOverlay(f source.File, version float64, data []byte) {
	s.overlayMu.Lock()
	defer func() {
		s.overlayMu.Unlock()
		s.filesWatchMap.Notify(f.URI(), source.Change)
	}()

	if data == nil {
		delete(s.overlays, f.URI())
		return
	}

	s.overlays[f.URI()] = &overlay{
		session: s,
		uri:     f.URI(),
		kind:    f.Kind(),
		data:    data,
		hash:    hashContents(data),
		version: version,
	}
}

func (s *session) clearOverlay(uri span.URI) {
	s.overlayMu.Lock()
	defer s.overlayMu.Unlock()

	delete(s.overlays, uri)
}

// openOverlay adds the file content to the overlay.
// It also checks if the provided content is equivalent to the file's content on disk.
func (s *session) openOverlay(ctx context.Context, uri span.URI, kind source.FileKind, version float64, data []byte) {
	s.overlayMu.Lock()
	defer func() {
		s.overlayMu.Unlock()
		s.filesWatchMap.Notify(uri, source.Open)
	}()
	s.overlays[uri] = &overlay{
		session: s,
		uri:     uri,
		kind:    kind,
		data:    data,
		hash:    hashContents(data),
		version: version,
	}
	// If the file is on disk, check if its content is the same as the overlay.
	if _, hash, err := s.cache.GetFile(uri, kind).Read(ctx); err == nil {
		if hash == s.overlays[uri].hash {
			s.overlays[uri].sameContentOnDisk = true
		}
	}
}

func (s *session) readOverlay(uri span.URI) *overlay {
	s.overlayMu.Lock()
	defer s.overlayMu.Unlock()

	// We might have the content saved in an overlay.
	if overlay, ok := s.overlays[uri]; ok {
		return overlay
	}
	return nil
}

func (s *session) buildOverlay() map[string][]byte {
	s.overlayMu.Lock()
	defer s.overlayMu.Unlock()

	overlays := make(map[string][]byte)
	for uri, overlay := range s.overlays {
		if overlay.sameContentOnDisk {
			continue
		}
		overlays[uri.Filename()] = overlay.data
	}
	return overlays
}

func (s *session) DidChangeOutOfBand(ctx context.Context, uri span.URI, action source.FileAction) bool {
	return s.filesWatchMap.Notify(uri, action)
}

func (o *overlay) FileSystem() source.FileSystem {
	return o.session
}

func (o *overlay) Identity() source.FileIdentity {
	return source.FileIdentity{
		URI:        o.uri,
		Identifier: o.hash,
		Version:    o.version,
		Kind:       o.kind,
	}
}
func (o *overlay) Read(ctx context.Context) ([]byte, string, error) {
	return o.data, o.hash, nil
}

type debugSession struct{ *session }

func (s debugSession) ID() string         { return s.id }
func (s debugSession) Cache() debug.Cache { return debugCache{s.cache} }
func (s debugSession) Files() []*debug.File {
	var files []*debug.File
	seen := make(map[span.URI]*debug.File)
	s.openFiles.Range(func(key interface{}, value interface{}) bool {
		uri, ok := key.(span.URI)
		if ok {
			f := &debug.File{Session: s, URI: uri}
			seen[uri] = f
			files = append(files, f)
		}
		return true
	})
	s.overlayMu.Lock()
	defer s.overlayMu.Unlock()
	for _, overlay := range s.overlays {
		f, ok := seen[overlay.uri]
		if !ok {
			f = &debug.File{Session: s, URI: overlay.uri}
			seen[overlay.uri] = f
			files = append(files, f)
		}
		f.Data = string(overlay.data)
		f.Error = nil
		f.Hash = overlay.hash
	}
	sort.Slice(files, func(i int, j int) bool {
		return files[i].URI < files[j].URI
	})
	return files
}

func (s debugSession) File(hash string) *debug.File {
	s.overlayMu.Lock()
	defer s.overlayMu.Unlock()
	for _, overlay := range s.overlays {
		if overlay.hash == hash {
			return &debug.File{
				Session: s,
				URI:     overlay.uri,
				Data:    string(overlay.data),
				Error:   nil,
				Hash:    overlay.hash,
			}
		}
	}
	return &debug.File{
		Session: s,
		Hash:    hash,
	}
}
