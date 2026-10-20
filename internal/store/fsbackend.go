package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/italypaleale/go-kit/objstore"
)

// Options a file system DSN can carry after a "?", which set the permissions of what the backend writes
const (
	fsOptionFileMode = "fileMode"
	fsOptionDirMode  = "dirMode"
)

// Permissions the file system backend gives what it creates unless the DSN says otherwise, which keep the objects to the user the service runs as
const (
	defaultFileMode fs.FileMode = 0o600
	defaultDirMode  fs.FileMode = 0o750
)

// newBackend creates the backend a DSN describes
//
// A file system DSN can end with "?fileMode=0644&dirMode=0755", which is what a web server running as another user needs to serve the files straight from the folder
// The options are read here rather than by the backend, which creates every file and folder with the default permissions
func newBackend(dsn string) (objstore.Backend, error) {
	dsn = strings.TrimSpace(dsn)
	scheme, value := objstore.Scheme(dsn)
	switch scheme {
	case "", objstore.SchemeFS, objstore.SchemeLocal:
		// Handled below
	default:
		// The caller says which store the error is about
		return objstore.New(dsn, objstore.Options{}) //nolint:wrapcheck
	}

	// Validate the options before creating the folder, so a typo in them doesn't leave a folder behind
	path, query, _ := strings.Cut(value, "?")
	fileMode, dirMode, err := parseFSModes(query)
	if err != nil {
		return nil, err
	}

	backend, err := objstore.NewFS(path)
	if err != nil {
		return nil, fmt.Errorf("failed to create the local storage backend: %w", err)
	}

	// The default permissions are the ones the backend already uses, so it's left as it is
	if fileMode == defaultFileMode && dirMode == defaultDirMode {
		return backend, nil
	}

	// The root was created with the default permissions, like every folder the backend creates
	err = os.Chmod(backend.Root(), dirMode)
	if err != nil {
		return nil, fmt.Errorf("failed to set the permissions of '%s': %w", backend.Root(), err)
	}

	return &fsBackend{FSBackend: backend, fileMode: fileMode, dirMode: dirMode}, nil
}

// Read the permissions a file system DSN asks for, falling back to the defaults for the ones it leaves out
func parseFSModes(query string) (fileMode fs.FileMode, dirMode fs.FileMode, err error) {
	fileMode = defaultFileMode
	dirMode = defaultDirMode
	if query == "" {
		return fileMode, dirMode, nil
	}

	values, err := url.ParseQuery(query)
	if err != nil {
		return 0, 0, fmt.Errorf("the options of the local storage DSN are invalid: %w", err)
	}

	// An option that isn't known is more likely a typo than something to ignore
	for key := range values {
		if key != fsOptionFileMode && key != fsOptionDirMode {
			return 0, 0, fmt.Errorf("the local storage DSN has the unknown option '%s': the options are '%s' and '%s'", key, fsOptionFileMode, fsOptionDirMode)
		}
	}

	if values.Has(fsOptionFileMode) {
		fileMode, err = parseMode(values.Get(fsOptionFileMode))
		if err != nil {
			return 0, 0, fmt.Errorf("option '%s' of the local storage DSN is invalid: %w", fsOptionFileMode, err)
		}
	}
	if values.Has(fsOptionDirMode) {
		dirMode, err = parseMode(values.Get(fsOptionDirMode))
		if err != nil {
			return 0, 0, fmt.Errorf("option '%s' of the local storage DSN is invalid: %w", fsOptionDirMode, err)
		}
	}

	// The service reads back what it writes, and creates files in the folders it writes to, so it can't give those permissions away from itself
	if fileMode&0o600 != 0o600 {
		return 0, 0, fmt.Errorf("option '%s' of the local storage DSN is %04o, which doesn't let the service read and write its own files: it needs 0600 at least", fsOptionFileMode, fileMode)
	}
	if dirMode&0o700 != 0o700 {
		return 0, 0, fmt.Errorf("option '%s' of the local storage DSN is %04o, which doesn't let the service use its own folders: it needs 0700 at least", fsOptionDirMode, dirMode)
	}

	return fileMode, dirMode, nil
}

// Parse permissions written in octal, such as "0644", "644" or "0o644"
func parseMode(value string) (fs.FileMode, error) {
	value = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(value)), "0o")
	if value == "" {
		return 0, errors.New("it is empty")
	}
	mode, err := strconv.ParseUint(value, 8, 32)
	if err != nil {
		return 0, fmt.Errorf("'%s' is not an octal number such as 0644", value)
	}
	if mode > 0o777 {
		return 0, fmt.Errorf("'%04o' is not a permission between 0000 and 0777", mode)
	}
	return fs.FileMode(mode), nil
}

// fsBackend is the file system backend with the permissions a DSN asked for
// Writes go through the go-kit backend, which creates every file and folder with the default permissions, and are given the configured ones right after
type fsBackend struct {
	*objstore.FSBackend

	fileMode fs.FileMode
	dirMode  fs.FileMode
}

// Put writes an object, then gives it and the folders above it the configured permissions
// A reader that finds the file through a manifest never sees it before that, because the manifest is written after the files it lists
func (b *fsBackend) Put(ctx context.Context, key string, body io.Reader, maxSize int64, contentType string) (objstore.PutResult, error) {
	res, err := b.FSBackend.Put(ctx, key, body, maxSize, contentType)
	if err != nil {
		//nolint:wrapcheck
		return res, err
	}

	path, err := b.Path(key)
	if err != nil {
		return objstore.PutResult{}, fmt.Errorf("failed to resolve the path of '%s': %w", key, err)
	}
	err = os.Chmod(path, b.fileMode)
	if err != nil {
		return objstore.PutResult{}, fmt.Errorf("failed to set the permissions of '%s': %w", path, err)
	}

	// The folders between the root and the file may have just been created, with the default permissions
	root := b.Root()
	for dir := filepath.Dir(path); dir != root && strings.HasPrefix(dir, root+string(filepath.Separator)); dir = filepath.Dir(dir) {
		err = os.Chmod(dir, b.dirMode)
		if err != nil {
			return objstore.PutResult{}, fmt.Errorf("failed to set the permissions of '%s': %w", dir, err)
		}
	}

	return res, nil
}

// Description returns the root folder objects are stored in, along with the permissions they get
func (b *fsBackend) Description() string {
	return fmt.Sprintf("%s, with permissions %04o for files and %04o for folders", b.FSBackend.Description(), b.fileMode, b.dirMode)
}
