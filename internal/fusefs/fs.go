// Package fusefs is the FUSE overlay that mirrors a backing directory and
// injects virtual slash-command files under commands/. See spec 02.
package fusefs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	gofs "github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/ralt/outpost/internal/logging"
)

// WriteEvent is fired by the FUSE layer for every write into a session .jsonl.
type WriteEvent struct {
	Munged string // project directory name (e.g. "-home-alice-...")
	ID     string // session id, the .jsonl basename without extension
	Offset int64
	Length int
}

// SessionLifecycleEvent reports create/unlink for session files.
type SessionLifecycleEvent struct {
	Munged string
	ID     string
	Kind   string // "create" | "unlink"
}

// Sink receives the FUSE write hook signals. All methods are non-blocking on
// the FUSE side — implementations must not call back into the FS.
type Sink interface {
	OnWrite(WriteEvent)
	OnSession(SessionLifecycleEvent)
}

// nullSink discards everything; used until the sync engine attaches.
type nullSink struct{}

func (nullSink) OnWrite(WriteEvent)              {}
func (nullSink) OnSession(SessionLifecycleEvent) {}

// FS is the mounted overlay. Construct with New, then Mount.
type FS struct {
	BackingDir   string
	MountDir     string
	Log          *slog.Logger
	startTime    time.Time
	virtuals     map[string][]byte
	sink         Sink
	sinkMu       sync.RWMutex
	server       *fuse.Server
}

// New constructs an overlay. virtuals is a name→content map keyed by file
// basename (e.g. "send-away.md").
func New(backing, mount string, virtuals map[string][]byte, log *slog.Logger) *FS {
	if log == nil {
		log = slog.Default()
	}
	return &FS{
		BackingDir: backing,
		MountDir:   mount,
		Log:        logging.WithComponent(log, logging.CompFUSE),
		startTime:  time.Now(),
		virtuals:   virtuals,
		sink:       nullSink{},
	}
}

// SetSink installs (or replaces) the receiver for write-hook events. Safe to
// call any time, including before/after Mount.
func (f *FS) SetSink(s Sink) {
	f.sinkMu.Lock()
	defer f.sinkMu.Unlock()
	if s == nil {
		f.sink = nullSink{}
		return
	}
	f.sink = s
}

func (f *FS) currentSink() Sink {
	f.sinkMu.RLock()
	defer f.sinkMu.RUnlock()
	return f.sink
}

// Mount validates preconditions, ensures dirs exist, and brings the FUSE mount
// up. Blocks until the kernel has accepted the mount. Returns a function to
// unmount that the caller must call exactly once on shutdown.
func (f *FS) Mount() (unmount func() error, err error) {
	if err := os.MkdirAll(f.BackingDir, 0o700); err != nil {
		return nil, fmt.Errorf("fuse: mkdir backing: %w", err)
	}
	if err := os.MkdirAll(f.MountDir, 0o700); err != nil {
		return nil, fmt.Errorf("fuse: mkdir mount: %w", err)
	}
	if err := f.checkMountSafety(); err != nil {
		return nil, err
	}

	root := &dirNode{root: f, relPath: ""}
	opts := &gofs.Options{
		MountOptions: fuse.MountOptions{
			Name:   "outpost",
			FsName: "outpost",
			AllowOther: false,
			DisableXAttrs: false,
		},
		AttrTimeout:  ptrDuration(time.Second),
		EntryTimeout: ptrDuration(time.Second),
	}
	srv, err := gofs.Mount(f.MountDir, root, opts)
	if err != nil {
		return nil, fmt.Errorf("fuse: mount %s: %w", f.MountDir, err)
	}
	f.server = srv
	f.Log.Info("fuse mount up", "mount", f.MountDir, "backing", f.BackingDir)
	return func() error {
		if err := srv.Unmount(); err != nil {
			return fmt.Errorf("fuse: unmount: %w", err)
		}
		f.Log.Info("fuse unmounted", "mount", f.MountDir)
		return nil
	}, nil
}

// Wait blocks until the FUSE loop exits (kernel unmount or Unmount call).
func (f *FS) Wait() {
	if f.server != nil {
		f.server.Wait()
	}
}

func ptrDuration(d time.Duration) *time.Duration { return &d }

func (f *FS) checkMountSafety() error {
	absBack, _ := filepath.Abs(f.BackingDir)
	absMnt, _ := filepath.Abs(f.MountDir)
	if absBack == absMnt {
		return errors.New("fuse: backing and mount must differ")
	}
	if strings.HasPrefix(absMnt+string(filepath.Separator), absBack+string(filepath.Separator)) {
		return errors.New("fuse: mountpoint is inside the backing dir (would loop)")
	}
	// Mountpoint non-empty + backing empty → would lose data via shadowing.
	mEntries, err := os.ReadDir(f.MountDir)
	if err != nil {
		return fmt.Errorf("fuse: read mount: %w", err)
	}
	if len(mEntries) > 0 {
		bEntries, err := os.ReadDir(f.BackingDir)
		if err != nil {
			return fmt.Errorf("fuse: read backing: %w", err)
		}
		if len(bEntries) == 0 {
			return fmt.Errorf("fuse: mountpoint %s has content but backing %s is empty — refusing to mount (would shadow user data)", f.MountDir, f.BackingDir)
		}
	}
	return nil
}

// ── nodes ───────────────────────────────────────────────────────────

// dirNode is the catch-all loopback dir node. relPath is the path relative to
// the mountpoint root, using forward slashes. The empty string is the root.
type dirNode struct {
	gofs.Inode
	root    *FS
	relPath string
}

var (
	_ gofs.NodeOnAdder    = (*dirNode)(nil)
	_ gofs.NodeLookuper   = (*dirNode)(nil)
	_ gofs.NodeReaddirer  = (*dirNode)(nil)
	_ gofs.NodeGetattrer  = (*dirNode)(nil)
	_ gofs.NodeCreater    = (*dirNode)(nil)
	_ gofs.NodeMkdirer    = (*dirNode)(nil)
	_ gofs.NodeUnlinker   = (*dirNode)(nil)
	_ gofs.NodeRmdirer    = (*dirNode)(nil)
	_ gofs.NodeRenamer    = (*dirNode)(nil)
	_ gofs.NodeSymlinker  = (*dirNode)(nil)
	_ gofs.NodeLinker     = (*dirNode)(nil)
	_ gofs.NodeReadlinker = (*dirNode)(nil)
	_ gofs.NodeStatfser   = (*dirNode)(nil)
	_ gofs.NodeSetattrer  = (*dirNode)(nil)
)

func (n *dirNode) OnAdd(ctx context.Context) {}

func (n *dirNode) backingPath() string {
	return filepath.Join(n.root.BackingDir, filepath.FromSlash(n.relPath))
}

func (n *dirNode) joinRel(name string) string {
	if n.relPath == "" {
		return name
	}
	return n.relPath + "/" + name
}

// Statfs forwards to the backing filesystem so df reports something sensible.
func (n *dirNode) Statfs(ctx context.Context, out *fuse.StatfsOut) syscall.Errno {
	var st syscall.Statfs_t
	if err := syscall.Statfs(n.backingPath(), &st); err != nil {
		return toErrno(err)
	}
	out.FromStatfsT(&st)
	return 0
}

func (n *dirNode) Getattr(ctx context.Context, _ gofs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	var st syscall.Stat_t
	if err := syscall.Lstat(n.backingPath(), &st); err != nil {
		return toErrno(err)
	}
	out.FromStat(&st)
	return 0
}

func (n *dirNode) Setattr(ctx context.Context, fh gofs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	p := n.backingPath()
	if size, ok := in.GetSize(); ok {
		if err := syscall.Truncate(p, int64(size)); err != nil {
			return toErrno(err)
		}
	}
	if mode, ok := in.GetMode(); ok {
		if err := syscall.Chmod(p, mode); err != nil {
			return toErrno(err)
		}
	}
	if uid, ok := in.GetUID(); ok {
		gid := -1
		if g, ok := in.GetGID(); ok {
			gid = int(g)
		}
		if err := syscall.Chown(p, int(uid), gid); err != nil {
			return toErrno(err)
		}
	} else if gid, ok := in.GetGID(); ok {
		if err := syscall.Chown(p, -1, int(gid)); err != nil {
			return toErrno(err)
		}
	}
	if mt, ok := in.GetMTime(); ok {
		at := mt
		if a, ok := in.GetATime(); ok {
			at = a
		}
		if err := os.Chtimes(p, at, mt); err != nil {
			return toErrno(err)
		}
	}
	return n.Getattr(ctx, fh, out)
}

func (n *dirNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*gofs.Inode, syscall.Errno) {
	child := filepath.Join(n.backingPath(), name)
	var st syscall.Stat_t
	if err := syscall.Lstat(child, &st); err != nil {
		// Virtual commands fallback.
		if errors.Is(err, syscall.ENOENT) && n.relPath == "commands" {
			if b, ok := n.root.virtuals[name]; ok {
				ch := n.NewInode(ctx, &virtualFile{root: n.root, content: b}, gofs.StableAttr{Mode: fuse.S_IFREG})
				fillVirtualEntry(out, b, n.root.startTime)
				return ch, 0
			}
		}
		return nil, toErrno(err)
	}
	out.FromStat(&st)
	out.Attr.FromStat(&st)
	mode := uint32(st.Mode) & syscall.S_IFMT
	rel := n.joinRel(name)
	switch mode {
	case syscall.S_IFDIR:
		ch := n.NewInode(ctx, &dirNode{root: n.root, relPath: rel}, gofs.StableAttr{Mode: fuse.S_IFDIR, Ino: st.Ino})
		return ch, 0
	case syscall.S_IFLNK:
		ch := n.NewInode(ctx, &symlinkNode{root: n.root, relPath: rel}, gofs.StableAttr{Mode: fuse.S_IFLNK, Ino: st.Ino})
		return ch, 0
	default:
		ch := n.NewInode(ctx, &fileNode{root: n.root, relPath: rel}, gofs.StableAttr{Mode: fuse.S_IFREG, Ino: st.Ino})
		return ch, 0
	}
}

func (n *dirNode) Readdir(ctx context.Context) (gofs.DirStream, syscall.Errno) {
	entries, err := os.ReadDir(n.backingPath())
	if err != nil {
		return nil, toErrno(err)
	}
	seen := map[string]struct{}{}
	out := make([]fuse.DirEntry, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		var ino uint64
		var mode uint32
		if err == nil {
			if s, ok := info.Sys().(*syscall.Stat_t); ok {
				ino = s.Ino
				mode = uint32(s.Mode)
			}
		}
		if mode == 0 {
			mode = fileModeFromOSMode(e.Type())
		}
		out = append(out, fuse.DirEntry{Name: e.Name(), Ino: ino, Mode: mode})
		seen[e.Name()] = struct{}{}
	}
	if n.relPath == "commands" {
		for name := range n.root.virtuals {
			if _, exists := seen[name]; exists {
				continue
			}
			out = append(out, fuse.DirEntry{Name: name, Mode: fuse.S_IFREG | 0o444})
		}
	}
	return gofs.NewListDirStream(out), 0
}

func fileModeFromOSMode(m os.FileMode) uint32 {
	switch {
	case m.IsDir():
		return fuse.S_IFDIR
	case m&os.ModeSymlink != 0:
		return fuse.S_IFLNK
	default:
		return fuse.S_IFREG
	}
}

func (n *dirNode) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*gofs.Inode, syscall.Errno) {
	p := filepath.Join(n.backingPath(), name)
	if err := syscall.Mkdir(p, mode); err != nil {
		return nil, toErrno(err)
	}
	var st syscall.Stat_t
	if err := syscall.Lstat(p, &st); err != nil {
		return nil, toErrno(err)
	}
	out.FromStat(&st)
	ch := n.NewInode(ctx, &dirNode{root: n.root, relPath: n.joinRel(name)}, gofs.StableAttr{Mode: fuse.S_IFDIR, Ino: st.Ino})
	return ch, 0
}

func (n *dirNode) Rmdir(ctx context.Context, name string) syscall.Errno {
	if n.relPath == "commands" {
		if _, ok := n.root.virtuals[name]; ok {
			return syscall.EROFS
		}
	}
	if err := syscall.Rmdir(filepath.Join(n.backingPath(), name)); err != nil {
		return toErrno(err)
	}
	return 0
}

func (n *dirNode) Unlink(ctx context.Context, name string) syscall.Errno {
	if n.relPath == "commands" {
		// Block deletion of virtuals only if the backing has no shadow.
		if _, ok := n.root.virtuals[name]; ok {
			if _, err := os.Lstat(filepath.Join(n.backingPath(), name)); err != nil {
				return syscall.EROFS
			}
		}
	}
	p := filepath.Join(n.backingPath(), name)
	if err := syscall.Unlink(p); err != nil {
		return toErrno(err)
	}
	if munged, id, ok := classifySessionPath(n.joinRel(name)); ok {
		n.root.currentSink().OnSession(SessionLifecycleEvent{Munged: munged, ID: id, Kind: "unlink"})
	}
	return 0
}

func (n *dirNode) Rename(ctx context.Context, name string, newParent gofs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	np, ok := newParent.(*dirNode)
	if !ok {
		return syscall.EXDEV
	}
	src := filepath.Join(n.backingPath(), name)
	dst := filepath.Join(np.backingPath(), newName)
	if err := os.Rename(src, dst); err != nil {
		return toErrno(err)
	}
	return 0
}

func (n *dirNode) Symlink(ctx context.Context, target, name string, out *fuse.EntryOut) (*gofs.Inode, syscall.Errno) {
	p := filepath.Join(n.backingPath(), name)
	if err := os.Symlink(target, p); err != nil {
		return nil, toErrno(err)
	}
	var st syscall.Stat_t
	if err := syscall.Lstat(p, &st); err != nil {
		return nil, toErrno(err)
	}
	out.FromStat(&st)
	ch := n.NewInode(ctx, &symlinkNode{root: n.root, relPath: n.joinRel(name)}, gofs.StableAttr{Mode: fuse.S_IFLNK, Ino: st.Ino})
	return ch, 0
}

func (n *dirNode) Link(ctx context.Context, target gofs.InodeEmbedder, name string, out *fuse.EntryOut) (*gofs.Inode, syscall.Errno) {
	tn, ok := target.(*fileNode)
	if !ok {
		return nil, syscall.EXDEV
	}
	p := filepath.Join(n.backingPath(), name)
	if err := os.Link(filepath.Join(tn.root.BackingDir, tn.relPath), p); err != nil {
		return nil, toErrno(err)
	}
	var st syscall.Stat_t
	if err := syscall.Lstat(p, &st); err != nil {
		return nil, toErrno(err)
	}
	out.FromStat(&st)
	ch := n.NewInode(ctx, &fileNode{root: n.root, relPath: n.joinRel(name)}, gofs.StableAttr{Mode: fuse.S_IFREG, Ino: st.Ino})
	return ch, 0
}

func (n *dirNode) Readlink(ctx context.Context) ([]byte, syscall.Errno) {
	return nil, syscall.EINVAL
}

func (n *dirNode) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (*gofs.Inode, gofs.FileHandle, uint32, syscall.Errno) {
	if n.relPath == "commands" {
		if _, ok := n.root.virtuals[name]; ok {
			// Allow create — backing entry shadows the virtual one.
		}
	}
	p := filepath.Join(n.backingPath(), name)
	fd, err := syscall.Open(p, int(flags)|syscall.O_CREAT, mode)
	if err != nil {
		return nil, nil, 0, toErrno(err)
	}
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		_ = syscall.Close(fd)
		return nil, nil, 0, toErrno(err)
	}
	out.FromStat(&st)
	rel := n.joinRel(name)
	ch := n.NewInode(ctx, &fileNode{root: n.root, relPath: rel}, gofs.StableAttr{Mode: fuse.S_IFREG, Ino: st.Ino})
	fh := newFileHandle(fd, n.root, rel)
	if munged, id, ok := classifySessionPath(rel); ok {
		n.root.currentSink().OnSession(SessionLifecycleEvent{Munged: munged, ID: id, Kind: "create"})
	}
	return ch, fh, 0, 0
}

// ── symlink ─────────────────────────────────────────────────────────

type symlinkNode struct {
	gofs.Inode
	root    *FS
	relPath string
}

var (
	_ gofs.NodeReadlinker = (*symlinkNode)(nil)
	_ gofs.NodeGetattrer  = (*symlinkNode)(nil)
)

func (s *symlinkNode) Readlink(ctx context.Context) ([]byte, syscall.Errno) {
	target, err := os.Readlink(filepath.Join(s.root.BackingDir, s.relPath))
	if err != nil {
		return nil, toErrno(err)
	}
	return []byte(target), 0
}

func (s *symlinkNode) Getattr(ctx context.Context, _ gofs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	var st syscall.Stat_t
	if err := syscall.Lstat(filepath.Join(s.root.BackingDir, s.relPath), &st); err != nil {
		return toErrno(err)
	}
	out.FromStat(&st)
	return 0
}

// ── file ────────────────────────────────────────────────────────────

type fileNode struct {
	gofs.Inode
	root    *FS
	relPath string
}

var (
	_ gofs.NodeOpener    = (*fileNode)(nil)
	_ gofs.NodeGetattrer = (*fileNode)(nil)
	_ gofs.NodeSetattrer = (*fileNode)(nil)
)

func (n *fileNode) Open(ctx context.Context, flags uint32) (gofs.FileHandle, uint32, syscall.Errno) {
	p := filepath.Join(n.root.BackingDir, n.relPath)
	fd, err := syscall.Open(p, int(flags), 0)
	if err != nil {
		return nil, 0, toErrno(err)
	}
	return newFileHandle(fd, n.root, n.relPath), 0, 0
}

func (n *fileNode) Getattr(ctx context.Context, _ gofs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	var st syscall.Stat_t
	if err := syscall.Lstat(filepath.Join(n.root.BackingDir, n.relPath), &st); err != nil {
		return toErrno(err)
	}
	out.FromStat(&st)
	return 0
}

func (n *fileNode) Setattr(ctx context.Context, fh gofs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	p := filepath.Join(n.root.BackingDir, n.relPath)
	if size, ok := in.GetSize(); ok {
		if err := syscall.Truncate(p, int64(size)); err != nil {
			return toErrno(err)
		}
	}
	if mode, ok := in.GetMode(); ok {
		if err := syscall.Chmod(p, mode); err != nil {
			return toErrno(err)
		}
	}
	if mt, ok := in.GetMTime(); ok {
		at := mt
		if a, ok := in.GetATime(); ok {
			at = a
		}
		if err := os.Chtimes(p, at, mt); err != nil {
			return toErrno(err)
		}
	}
	return n.Getattr(ctx, fh, out)
}

// ── file handle ─────────────────────────────────────────────────────

type fileHandle struct {
	fd     int
	root   *FS
	rel    string
	mu     sync.Mutex
	munged string
	id     string
	isSess bool
}

var (
	_ gofs.FileReader    = (*fileHandle)(nil)
	_ gofs.FileWriter    = (*fileHandle)(nil)
	_ gofs.FileLseeker   = (*fileHandle)(nil)
	_ gofs.FileFlusher   = (*fileHandle)(nil)
	_ gofs.FileReleaser  = (*fileHandle)(nil)
	_ gofs.FileFsyncer   = (*fileHandle)(nil)
	_ gofs.FileGetattrer = (*fileHandle)(nil)
)

func newFileHandle(fd int, root *FS, rel string) *fileHandle {
	h := &fileHandle{fd: fd, root: root, rel: rel}
	if m, id, ok := classifySessionPath(rel); ok {
		h.munged = m
		h.id = id
		h.isSess = true
	}
	return h
}

func (h *fileHandle) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	h.mu.Lock()
	defer h.mu.Unlock()
	n, err := syscall.Pread(h.fd, dest, off)
	if err != nil {
		return nil, toErrno(err)
	}
	return fuse.ReadResultData(dest[:n]), 0
}

func (h *fileHandle) Write(ctx context.Context, data []byte, off int64) (uint32, syscall.Errno) {
	h.mu.Lock()
	n, err := syscall.Pwrite(h.fd, data, off)
	h.mu.Unlock()
	if err != nil {
		return 0, toErrno(err)
	}
	if h.isSess && n > 0 {
		h.root.currentSink().OnWrite(WriteEvent{Munged: h.munged, ID: h.id, Offset: off, Length: n})
	}
	return uint32(n), 0
}

func (h *fileHandle) Lseek(ctx context.Context, off uint64, whence uint32) (uint64, syscall.Errno) {
	n, err := syscall.Seek(h.fd, int64(off), int(whence))
	if err != nil {
		return 0, toErrno(err)
	}
	return uint64(n), 0
}

func (h *fileHandle) Flush(ctx context.Context) syscall.Errno {
	if err := syscall.Fsync(h.fd); err != nil {
		// Some filesystems don't support fsync on every fd type.
		if errors.Is(err, syscall.EINVAL) {
			return 0
		}
		return toErrno(err)
	}
	return 0
}

func (h *fileHandle) Fsync(ctx context.Context, flags uint32) syscall.Errno {
	if err := syscall.Fsync(h.fd); err != nil {
		return toErrno(err)
	}
	return 0
}

func (h *fileHandle) Release(ctx context.Context) syscall.Errno {
	if err := syscall.Close(h.fd); err != nil {
		return toErrno(err)
	}
	return 0
}

func (h *fileHandle) Getattr(ctx context.Context, out *fuse.AttrOut) syscall.Errno {
	var st syscall.Stat_t
	if err := syscall.Fstat(h.fd, &st); err != nil {
		return toErrno(err)
	}
	out.FromStat(&st)
	return 0
}

// ── virtual file ────────────────────────────────────────────────────

type virtualFile struct {
	gofs.Inode
	root    *FS
	content []byte
}

var (
	_ gofs.NodeOpener   = (*virtualFile)(nil)
	_ gofs.NodeGetattrer = (*virtualFile)(nil)
	_ gofs.NodeSetattrer = (*virtualFile)(nil)
)

func (v *virtualFile) Open(ctx context.Context, flags uint32) (gofs.FileHandle, uint32, syscall.Errno) {
	// Writes are rejected.
	access := flags & uint32(os.O_RDWR|os.O_WRONLY|os.O_TRUNC|os.O_APPEND)
	if access != 0 {
		return nil, 0, syscall.EROFS
	}
	return &virtualHandle{content: v.content}, fuse.FOPEN_KEEP_CACHE, 0
}

func (v *virtualFile) Getattr(ctx context.Context, _ gofs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	fillVirtualAttr(&out.Attr, v.content, v.root.startTime)
	return 0
}

func (v *virtualFile) Setattr(ctx context.Context, fh gofs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	return syscall.EROFS
}

type virtualHandle struct {
	content []byte
}

var (
	_ gofs.FileReader   = (*virtualHandle)(nil)
	_ gofs.FileReleaser = (*virtualHandle)(nil)
)

func (h *virtualHandle) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	if off >= int64(len(h.content)) {
		return fuse.ReadResultData(nil), 0
	}
	end := off + int64(len(dest))
	if end > int64(len(h.content)) {
		end = int64(len(h.content))
	}
	return fuse.ReadResultData(h.content[off:end]), 0
}

func (h *virtualHandle) Release(ctx context.Context) syscall.Errno { return 0 }

func fillVirtualEntry(out *fuse.EntryOut, content []byte, t time.Time) {
	fillVirtualAttr(&out.Attr, content, t)
	out.Mode = fuse.S_IFREG | 0o444
	out.Size = uint64(len(content))
}

func fillVirtualAttr(a *fuse.Attr, content []byte, t time.Time) {
	a.Mode = fuse.S_IFREG | 0o444
	a.Size = uint64(len(content))
	a.Nlink = 1
	a.Uid = uint32(os.Getuid())
	a.Gid = uint32(os.Getgid())
	a.Mtime = uint64(t.Unix())
	a.Atime = a.Mtime
	a.Ctime = a.Mtime
}

// classifySessionPath returns (munged, id, true) if rel is a session .jsonl,
// i.e. matches "projects/<munged>/<id>.jsonl" with no extra directory levels.
func classifySessionPath(rel string) (string, string, bool) {
	if !strings.HasPrefix(rel, "projects/") {
		return "", "", false
	}
	rest := strings.TrimPrefix(rel, "projects/")
	parts := strings.Split(rest, "/")
	if len(parts) != 2 {
		return "", "", false
	}
	if !strings.HasSuffix(parts[1], ".jsonl") {
		return "", "", false
	}
	id := strings.TrimSuffix(parts[1], ".jsonl")
	if id == "" {
		return "", "", false
	}
	return parts[0], id, true
}

func toErrno(err error) syscall.Errno {
	if err == nil {
		return 0
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno
	}
	if errors.Is(err, os.ErrNotExist) {
		return syscall.ENOENT
	}
	if errors.Is(err, os.ErrPermission) {
		return syscall.EACCES
	}
	return syscall.EIO
}

// ensure unused imports stay tidy
var _ = fmt.Sprintf
