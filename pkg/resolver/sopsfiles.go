package resolver

import (
	"context"
	"maps"
	"slices"

	"github.com/schjan/picolet/pkg/sops"
)

// sopsFileSet is the set of SOPS files (Fleet-relative paths) one resolve's
// sops:// refs reached. It belongs to one per-resolve Resolver copy and is
// written only by that resolve's serial rendering, so it needs no lock.
type sopsFileSet map[string]struct{}

// add records the file raw names; a ref that does not parse reaches no file.
func (s sopsFileSet) add(raw string) {
	if ref, err := sops.ParseRef(raw); err == nil {
		s[ref.Path] = struct{}{}
	}
}

// sorted returns the recorded paths in order; nil when none.
func (s sopsFileSet) sorted() []string {
	if len(s) == 0 {
		return nil
	}
	return slices.Sorted(maps.Keys(s))
}

// recordRefs wraps reader so that every ref it is asked for is recorded.
func (s sopsFileSet) recordRefs(reader SecretRefReader) SecretRefReader {
	return func(ctx context.Context, refs []string) (map[string]string, error) {
		for _, raw := range refs {
			s.add(raw)
		}
		return reader(ctx, refs)
	}
}

// sopsFileRecorder is a SopsReader that records the file of every ref it
// reads into files.
type sopsFileRecorder struct {
	reader SopsReader
	files  sopsFileSet
}

func (r sopsFileRecorder) ReadAll(ctx context.Context, refs []string) (map[string]string, error) {
	return r.files.recordRefs(r.reader.ReadAll)(ctx, refs)
}

func (r sopsFileRecorder) ReadData(ctx context.Context, ref string) (any, error) {
	r.files.add(ref)
	return r.reader.ReadData(ctx, ref)
}
