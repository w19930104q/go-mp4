package mp4

import (
	"errors"
	"fmt"
	"io"
	"sort"
)

// DefaultIndexCapacity is the default maximum number of boxes a BoxIndex
// keeps in memory when no explicit capacity is provided.
const DefaultIndexCapacity = 100000

var (
	// ErrIndexCapacityExceeded is returned when building a BoxIndex would
	// require more entries than its configured memory bound.
	ErrIndexCapacityExceeded = errors.New("mp4: box index capacity exceeded")

	// ErrBoxNotIndexed is returned when a lookup cannot find the requested
	// box in the index.
	ErrBoxNotIndexed = errors.New("mp4: box not found in index")
)

// IndexedBox is a single structural record held by a BoxIndex.
//
// It contains only the box's structural information and never the box
// payload, so the per-entry memory footprint is fixed regardless of box
// sizes. Info carries the same Context a sequential ReadBoxStructure scan
// would produce, and Path is the absolute box path from the file root.
type IndexedBox struct {
	// Ordinal is the zero-based position of the box in depth-first
	// (pre-order) traversal order, identical to the order in which
	// ReadBoxStructure visits boxes.
	Ordinal int

	// Info is the structural information of the box, including the
	// Context computed during the indexing scan.
	Info BoxInfo

	// Path is the absolute path from the root box to this box.
	Path BoxPath

	// ParentOrdinal is the Ordinal of the enclosing box, or -1 for a
	// top-level box.
	ParentOrdinal int

	// FirstChildOrdinal is the Ordinal of the first direct child, or -1
	// when the box has no indexed children.
	FirstChildOrdinal int
}

// BoxIndex is a memory-bounded structural index of an MP4 file that enables
// random access to boxes without re-scanning the file.
//
// A BoxIndex is created with BuildBoxIndex. Once built, locating a box by
// ordinal (BoxAt), file offset (BoxAtOffset) or path (Find / FindAll) does
// not scan the box structure again: results are served from the in-memory
// records and, when a payload is required, the underlying io.ReadSeeker is
// seeked directly to the box payload.
//
// The index stores only fixed-size structural records. The maximum number
// of records, and therefore the upper bound of its memory usage, is fixed
// at build time by the capacity option. Without an index, the behavior of
// all other APIs is unchanged.
type BoxIndex struct {
	r        io.ReadSeeker
	entries  []IndexedBox
	capacity int
}

// BuildBoxIndex scans r once in depth-first order and builds a structural
// index over every visited box.
//
// The scan follows exactly the same traversal as ReadBoxStructure, so a box
// located through the index has identical BoxInfo, Path and order as the box
// produced by a sequential scan. Subsequent lookups on the returned index do
// not re-scan the structure.
//
// The number of indexed boxes is bounded by options. When the bound would be
// exceeded, BuildBoxIndex fails with ErrIndexCapacityExceeded. Pass no
// option to use DefaultIndexCapacity.
func BuildBoxIndex(r io.ReadSeeker, opts ...BoxIndexOption) (*BoxIndex, error) {
	idx := &BoxIndex{
		r:        r,
		capacity: DefaultIndexCapacity,
	}
	for _, opt := range opts {
		opt(idx)
	}
	if idx.capacity < 0 {
		return nil, errors.New("mp4: index capacity must not be negative")
	}
	idx.entries = make([]IndexedBox, 0, 64)

	rec := &boxIndexRecorder{idx: idx}
	handler := func(h *ReadHandle) (interface{}, error) {
		// Expand every supported container so the whole tree is indexed.
		// Unsupported box types cannot be unmarshaled and a sequential scan
		// cannot descend into them either, so they are recorded as leaves.
		// The actual recording happens inside readBoxStructureFromInternal,
		// which has the authoritative BoxInfo and absolute Path.
		if !h.BoxInfo.IsSupportedType() {
			return nil, nil
		}
		vals, err := h.Expand()
		// An unsupported box version likewise has no determinable child
		// region, matching how a full-tree dump treats such a box as a leaf.
		if errors.Is(err, ErrUnsupportedBoxVersion) {
			return nil, nil
		}
		return vals, err
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	if _, err := readBoxStructure(r, 0, true, nil, Context{}, handler, nil, rec); err != nil {
		return nil, err
	}
	return idx, nil
}

// BoxIndexOption customizes a BoxIndex at build time.
type BoxIndexOption func(*BoxIndex)

// WithIndexCapacity sets the maximum number of box records kept in the
// index. This is a hard memory bound: building fails with
// ErrIndexCapacityExceeded if the file contains more boxes. A capacity of
// zero means an unlimited index.
func WithIndexCapacity(capacity int) BoxIndexOption {
	return func(idx *BoxIndex) {
		idx.capacity = capacity
	}
}

// Len returns the number of indexed boxes.
func (idx *BoxIndex) Len() int {
	return len(idx.entries)
}

// Capacity returns the configured maximum number of indexed boxes. A return
// value of zero means the index has no fixed bound.
func (idx *BoxIndex) Capacity() int {
	return idx.capacity
}

// BoxAt returns the box at the given zero-based depth-first ordinal.
// The ordinal space is identical to ReadBoxStructure visit order.
func (idx *BoxIndex) BoxAt(ordinal int) (*IndexedBox, error) {
	if ordinal < 0 || ordinal >= len(idx.entries) {
		return nil, fmt.Errorf("%w: ordinal=%d", ErrBoxNotIndexed, ordinal)
	}
	return &idx.entries[ordinal], nil
}

// BoxAtOffset returns the box whose start offset equals offset.
// Lookup is a binary search over the offset-sorted entries and performs no
// file scanning.
func (idx *BoxIndex) BoxAtOffset(offset uint64) (*IndexedBox, error) {
	n := len(idx.entries)
	i := sort.Search(n, func(i int) bool {
		return idx.entries[i].Info.Offset >= offset
	})
	if i >= n || idx.entries[i].Info.Offset != offset {
		return nil, fmt.Errorf("%w: offset=%d", ErrBoxNotIndexed, offset)
	}
	return &idx.entries[i], nil
}

// Children returns the direct children of box, in depth-first order.
func (idx *BoxIndex) Children(box *IndexedBox) []*IndexedBox {
	if box == nil || box.FirstChildOrdinal < 0 {
		return nil
	}
	children := make([]*IndexedBox, 0, 4)
	for i := box.FirstChildOrdinal; i < len(idx.entries); i++ {
		if idx.entries[i].ParentOrdinal != box.Ordinal {
			break
		}
		children = append(children, &idx.entries[i])
	}
	return children
}

// matchIndexedPath reports whether the absolute path of entry matches one
// of the requested paths using the same semantics as ExtractBoxes.
func matchIndexedPath(paths []BoxPath, path BoxPath) (forwardMatch bool, match bool) {
	for i := range paths {
		fm, m := path.compareWith(paths[i])
		forwardMatch = forwardMatch || fm
		match = match || m
	}
	return
}

// findAll returns every indexed box whose absolute path matches paths,
// preserving depth-first (index) order.
func (idx *BoxIndex) findAll(paths []BoxPath) []*IndexedBox {
	boxes := make([]*IndexedBox, 0, 8)
	for i := range idx.entries {
		entry := &idx.entries[i]
		if _, m := matchIndexedPath(paths, entry.Path); m {
			boxes = append(boxes, entry)
		}
	}
	return boxes
}

// Find returns the first box matching path, in depth-first order.
// It returns ErrBoxNotIndexed when no box matches.
func (idx *BoxIndex) Find(path BoxPath) (*IndexedBox, error) {
	if len(path) == 0 {
		return nil, errors.New("box path must not be empty")
	}
	boxes := idx.findAll([]BoxPath{path})
	if len(boxes) == 0 {
		return nil, fmt.Errorf("%w: path=%s", ErrBoxNotIndexed, pathString(path))
	}
	return boxes[0], nil
}

// FindAll returns every box matching any of the paths, in depth-first order.
func (idx *BoxIndex) FindAll(paths []BoxPath) ([]*IndexedBox, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	for i := range paths {
		if len(paths[i]) == 0 {
			return nil, errors.New("box path must not be empty")
		}
	}
	return idx.findAll(paths), nil
}

// findDescendants matches boxes beneath parent using paths relative to the
// parent, matching ReadBoxStructureFromInternal + ExtractBoxes semantics.
func (idx *BoxIndex) findDescendants(parent *IndexedBox, paths []BoxPath) []*IndexedBox {
	boxes := make([]*IndexedBox, 0, 8)
	prefixLen := len(parent.Path)
	for i := range idx.entries {
		entry := &idx.entries[i]
		if len(entry.Path) <= prefixLen {
			continue
		}
		// entry must be inside parent: its ancestor at parent's depth is parent.
		if !pathsEqual(entry.Path[:prefixLen], parent.Path) {
			continue
		}
		rel := entry.Path[prefixLen:]
		if _, m := matchIndexedPath(paths, rel); m {
			boxes = append(boxes, entry)
		}
	}
	return boxes
}

// ReadPayload reads and unmarshals the payload of an indexed box directly,
// seeking to its payload offset without scanning any surrounding structure.
// The result is identical to calling ReadHandle.ReadPayload during a
// sequential scan.
func (idx *BoxIndex) ReadPayload(box *IndexedBox) (IBox, uint64, error) {
	bi := box.Info
	if _, err := bi.SeekToPayload(idx.r); err != nil {
		return nil, 0, err
	}
	if err := checkPayloadSize(idx.r, &bi); err != nil {
		return nil, 0, err
	}
	payload, n, err := UnmarshalAny(idx.r, bi.Type, bi.Size-bi.HeaderSize, bi.Context)
	if err != nil {
		return nil, 0, err
	}
	return payload, n, nil
}

// ExtractBoxes returns the indexed boxes matching paths relative to parent.
// When parent is nil the paths are interpreted from the file root. This
// performs no structural re-scan: all matching is done over the index, and
// the returned BoxInfo values equal those of a sequential ExtractBoxes call.
func (idx *BoxIndex) ExtractBoxes(parent *IndexedBox, paths []BoxPath) ([]*BoxInfo, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	for i := range paths {
		if len(paths[i]) == 0 {
			return nil, errors.New("box path must not be empty")
		}
	}

	var matched []*IndexedBox
	if parent == nil {
		matched = idx.findAll(paths)
	} else {
		matched = idx.findDescendants(parent, paths)
	}

	bis := make([]*BoxInfo, 0, len(matched))
	for _, entry := range matched {
		bi := entry.Info
		bis = append(bis, &bi)
	}
	return bis, nil
}

// ExtractBox is the single-path variant of ExtractBoxes.
func (idx *BoxIndex) ExtractBox(parent *IndexedBox, path BoxPath) ([]*BoxInfo, error) {
	return idx.ExtractBoxes(parent, []BoxPath{path})
}

// ExtractBoxesWithPayload returns matching boxes together with their
// unmarshaled payload. Payloads are read by direct seek, never by
// re-scanning the structure.
func (idx *BoxIndex) ExtractBoxesWithPayload(parent *IndexedBox, paths []BoxPath) ([]*BoxInfoWithPayload, error) {
	var entries []*IndexedBox
	if parent == nil {
		if len(paths) == 0 {
			return nil, nil
		}
		for i := range paths {
			if len(paths[i]) == 0 {
				return nil, errors.New("box path must not be empty")
			}
		}
		entries = idx.findAll(paths)
	} else {
		entries = idx.findDescendants(parent, paths)
	}

	out := make([]*BoxInfoWithPayload, 0, len(entries))
	for _, entry := range entries {
		payload, _, err := idx.ReadPayload(entry)
		if err != nil {
			return nil, err
		}
		out = append(out, &BoxInfoWithPayload{
			Info:    entry.Info,
			Payload: payload,
		})
	}
	return out, nil
}

// ExtractBoxWithPayload is the single-path variant of
// ExtractBoxesWithPayload.
func (idx *BoxIndex) ExtractBoxWithPayload(parent *IndexedBox, path BoxPath) ([]*BoxInfoWithPayload, error) {
	return idx.ExtractBoxesWithPayload(parent, []BoxPath{path})
}

// boxIndexRecorder receives traversal callbacks while an index is built and
// appends fixed-size records, enforcing the capacity bound.
type boxIndexRecorder struct {
	idx *BoxIndex

	// depthStack maps a path depth to the ordinal of the most recently
	// recorded box at that depth. Boxes are visited in pre-order, so the
	// entry at depth-1 is always the current box's parent.
	depthStack []int
}

func (rec *boxIndexRecorder) record(bi *BoxInfo, path BoxPath) error {
	idx := rec.idx
	if idx.capacity != 0 && len(idx.entries) >= idx.capacity {
		return fmt.Errorf("%w: capacity=%d", ErrIndexCapacityExceeded, idx.capacity)
	}

	ordinal := len(idx.entries)
	depth := len(path)

	// Drop entries at this depth and below: their subtrees are complete.
	if len(rec.depthStack) > depth-1 {
		rec.depthStack = rec.depthStack[:depth-1]
	}

	parentOrdinal := -1
	if depth > 1 {
		parentOrdinal = rec.depthStack[depth-2]
	}
	rec.depthStack = append(rec.depthStack, ordinal)

	entry := IndexedBox{
		Ordinal:           ordinal,
		Info:              *bi,
		Path:              append(BoxPath(nil), path...),
		ParentOrdinal:     parentOrdinal,
		FirstChildOrdinal: -1,
	}
	idx.entries = append(idx.entries, entry)

	if parentOrdinal >= 0 && idx.entries[parentOrdinal].FirstChildOrdinal < 0 {
		idx.entries[parentOrdinal].FirstChildOrdinal = ordinal
	}
	return nil
}

func pathsEqual(a, b BoxPath) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func pathString(path BoxPath) string {
	s := ""
	for i, t := range path {
		if i > 0 {
			s += "/"
		}
		s += t.String()
	}
	return s
}
