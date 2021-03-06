package svfs

import (
	"os"
	"regexp"
	"sync"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"github.com/xlucas/swift"
	"golang.org/x/net/context"
)

const (
	objContentType    = "application/octet-stream"
	autoContentHeader = "X-Detect-Content-Type"
	manifestHeader    = "X-Object-Manifest"
	objectMetaHeader  = "X-Object-Meta-"
)

var (
	objectMtimeHeader = objectMetaHeader + "Mtime"
	segmentPathRegex  = regexp.MustCompile("^([^/]+)/(.*)$")
)

// Object is a node representing a swift object.
// It belongs to a container and segmented objects
// are bound to a container of segments.
type Object struct {
	name      string
	path      string
	so        *swift.Object
	sh        swift.Headers
	c         *swift.Container
	cs        *swift.Container
	p         *Directory
	m         sync.Mutex
	segmented bool
	writing   bool
}

// Attr fills the file attributes for an object node.
func (o *Object) Attr(ctx context.Context, a *fuse.Attr) (err error) {
	a.Size = o.size()
	a.BlockSize = uint32(BlockSize)
	a.Blocks = (a.Size / uint64(a.BlockSize)) * 8
	a.Mode = os.FileMode(DefaultMode)
	a.Gid = uint32(DefaultGID)
	a.Uid = uint32(DefaultUID)
	a.Mtime = getMtime(o.so, o.sh)
	a.Ctime = a.Mtime
	a.Crtime = a.Mtime
	return nil
}

// Export converts this object node as a direntry.
func (o *Object) Export() fuse.Dirent {
	return fuse.Dirent{
		Name: o.Name(),
		Type: fuse.DT_File,
	}
}

// Open returns the file handle associated with this object node.
func (o *Object) Open(ctx context.Context, req *fuse.OpenRequest, resp *fuse.OpenResponse) (fs.Handle, error) {
	return o.open(req.Flags, &resp.Flags)
}

// Setattr changes file attributes on the current node.
func (o *Object) Setattr(ctx context.Context, req *fuse.SetattrRequest, resp *fuse.SetattrResponse) error {
	// Change file size. Depending on the plaform, it may notably
	// be used by the kernel to truncate files instead of opening
	// them with O_TRUNC flag.
	if req.Valid.Size() {
		o.so.Bytes = int64(req.Size)
		if req.Size == 0 && o.segmented {
			return o.removeSegments()
		}
		return nil
	}

	if !ExtraAttr || !req.Valid.Mtime() {
		return fuse.ENOTSUP
	}

	// Change mtime
	if !req.Mtime.Equal(getMtime(o.so, o.sh)) {
		if o.writing {
			o.m.Lock()
			defer o.m.Unlock()
		}
		h := o.sh.ObjectMetadata().Headers(objectMetaHeader)
		o.sh[objectMtimeHeader] = formatTime(req.Mtime)
		h[objectMtimeHeader] = o.sh[objectMtimeHeader]
		return SwiftConnection.ObjectUpdate(o.c.Name, o.so.Name, h)
	}

	return nil
}

// Name gets the name of the underlying swift object.
func (o *Object) Name() string {
	return o.name
}

func (o *Object) copy(dir *Directory, name string) (copy *Object, err error) {
	if o.segmented {
		_, err = SwiftConnection.ManifestCopy(o.c.Name, o.path, dir.c.Name, dir.path+name, nil)
	} else {
		_, err = SwiftConnection.ObjectCopy(o.c.Name, o.path, dir.c.Name, dir.path+name, nil)
	}

	if err != nil {
		return nil, err
	}

	object := *o
	*object.so = *o.so
	object.c = dir.c
	object.cs = dir.cs
	object.p = dir
	object.name = name
	object.path = dir.path + name
	object.so.Name = dir.path + name

	directoryCache.Set(dir.c.Name, dir.path, name, &object)

	return &object, nil
}

func (o *Object) delete() error {
	directoryCache.Delete(o.c.Name, o.p.path, o.name)
	return SwiftConnection.ObjectDelete(o.c.Name, o.path)
}

func (o *Object) open(mode fuse.OpenFlags, flags *fuse.OpenResponseFlags) (*ObjectHandle, error) {
	oh := &ObjectHandle{
		target: o,
		create: mode&fuse.OpenCreate == fuse.OpenCreate,
	}

	// Unsupported flags
	if mode&fuse.OpenAppend == fuse.OpenAppend {
		return nil, fuse.ENOTSUP
	}

	// Supported flags
	if mode.IsReadOnly() {
		return oh, nil
	}
	if mode.IsWriteOnly() {
		o.m.Lock()
		changeCache.Add(o.c.Name, o.path, o)

		*flags |= fuse.OpenNonSeekable
		*flags |= fuse.OpenDirectIO

		return oh, nil
	}

	return nil, fuse.ENOTSUP
}

func (o *Object) rename(dir *Directory, name string) error {
	copy, err := o.copy(dir, name)
	if err != nil {
		return err
	}

	err = o.delete()
	if err != nil {
		return err
	}

	*o = *copy

	return nil
}

func (o *Object) removeSegments() error {
	o.segmented = false
	if err := deleteSegments(o.cs.Name, o.sh[manifestHeader]); err != nil {
		return err
	}
	delete(o.sh, manifestHeader)
	return nil
}

func (o *Object) size() uint64 {
	return uint64(o.so.Bytes)
}

var (
	_ Node             = (*Object)(nil)
	_ fs.Node          = (*Object)(nil)
	_ fs.NodeSetattrer = (*Object)(nil)
	_ fs.NodeOpener    = (*Object)(nil)
)
