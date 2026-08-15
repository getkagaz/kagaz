// Package tags reads and writes macOS Finder tags, which live in the
// com.apple.metadata:_kMDItemUserTags extended attribute as a binary plist
// array of strings. Finder tags are the vault's index: they are what makes
// `mdfind` and the Finder sidebar work with zero Kagaz software installed.
//
// Every operation degrades gracefully. On a filesystem without xattr support
// (many network mounts, and Linux CI) reads return no tags and writes report
// ErrUnsupported, which callers surface as a warning rather than a failure.
package tags

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"syscall"

	"github.com/pkg/xattr"
	"howett.net/plist"
)

// Attr is the extended attribute Finder uses for user tags.
const Attr = "com.apple.metadata:_kMDItemUserTags"

// ErrUnsupported means the filesystem cannot store extended attributes.
var ErrUnsupported = errors.New("extended attributes are not supported on this filesystem")

// Read returns the Finder tags on path, lowercased and sorted. A file with no
// tag attribute returns an empty slice and no error.
func Read(path string) ([]string, error) {
	data, err := xattr.LGet(path, Attr)
	if err != nil {
		if isMissing(err) {
			return nil, nil
		}
		if isUnsupported(err) {
			return nil, ErrUnsupported
		}
		return nil, err
	}
	return decode(data)
}

// Write replaces the Finder tags on path. Writing an empty set removes the
// attribute entirely, which is what Finder itself does.
func Write(path string, list []string) error {
	norm := Normalize(list)
	if len(norm) == 0 {
		err := xattr.LRemove(path, Attr)
		if err != nil && !isMissing(err) {
			if isUnsupported(err) {
				return ErrUnsupported
			}
			return err
		}
		return nil
	}
	data, err := encode(norm)
	if err != nil {
		return err
	}
	if err := xattr.LSet(path, Attr, data); err != nil {
		if isUnsupported(err) {
			return ErrUnsupported
		}
		return err
	}
	return nil
}

// Add adds tags to path, preserving existing ones.
func Add(path string, add ...string) error {
	cur, err := Read(path)
	if err != nil && !errors.Is(err, ErrUnsupported) {
		return err
	}
	return Write(path, append(cur, add...))
}

// Remove deletes tags from path.
func Remove(path string, remove ...string) error {
	cur, err := Read(path)
	if err != nil && !errors.Is(err, ErrUnsupported) {
		return err
	}
	drop := map[string]bool{}
	for _, r := range Normalize(remove) {
		drop[r] = true
	}
	var kept []string
	for _, t := range cur {
		if !drop[t] {
			kept = append(kept, t)
		}
	}
	return Write(path, kept)
}

// Apply adds and removes tags in one read-modify-write.
func Apply(path string, add, remove []string) error {
	cur, err := Read(path)
	if err != nil && !errors.Is(err, ErrUnsupported) {
		return err
	}
	drop := map[string]bool{}
	for _, r := range Normalize(remove) {
		drop[r] = true
	}
	var kept []string
	for _, t := range cur {
		if !drop[t] {
			kept = append(kept, t)
		}
	}
	return Write(path, append(kept, add...))
}

// Has reports whether path carries tag.
func Has(path, tag string) (bool, error) {
	cur, err := Read(path)
	if err != nil {
		return false, err
	}
	want := normalizeOne(tag)
	for _, t := range cur {
		if t == want {
			return true, nil
		}
	}
	return false, nil
}

// Copy transfers the tag set from src to dst. Used by the move engine, which
// copies bytes rather than renaming and so must carry metadata across itself.
func Copy(src, dst string) error {
	cur, err := Read(src)
	if err != nil {
		if errors.Is(err, ErrUnsupported) {
			return nil
		}
		return err
	}
	if len(cur) == 0 {
		return nil
	}
	err = Write(dst, cur)
	if errors.Is(err, ErrUnsupported) {
		return nil
	}
	return err
}

// Normalize lowercases, trims, de-duplicates and sorts a tag list. Finder tags
// are case-preserving but case-insensitive; Kagaz stores them lowercased so
// that filenames, tags and CLI filters all agree on one spelling.
func Normalize(list []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range list {
		n := normalizeOne(t)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func normalizeOne(t string) string {
	// Finder appends "\n<colour index>" to coloured tags; the colour is not part
	// of the name and is not something Kagaz manages.
	if i := strings.IndexByte(t, '\n'); i >= 0 {
		t = t[:i]
	}
	return strings.ToLower(strings.TrimSpace(t))
}

// decode parses the binary plist array Finder stores.
func decode(data []byte) ([]string, error) {
	if len(data) == 0 {
		return nil, nil
	}
	var raw []string
	if _, err := plist.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("decode Finder tags: %w", err)
	}
	return Normalize(raw), nil
}

// encode produces the binary plist Finder expects.
func encode(list []string) ([]byte, error) {
	var buf bytes.Buffer
	enc := plist.NewBinaryEncoder(&buf)
	if err := enc.Encode(list); err != nil {
		return nil, fmt.Errorf("encode Finder tags: %w", err)
	}
	return buf.Bytes(), nil
}

func isMissing(err error) bool {
	var xe *xattr.Error
	if errors.As(err, &xe) {
		err = xe.Err
	}
	return errors.Is(err, xattr.ENOATTR) || errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENODATA)
}

func isUnsupported(err error) bool {
	var xe *xattr.Error
	if errors.As(err, &xe) {
		err = xe.Err
	}
	return errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP) || errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EINVAL)
}
