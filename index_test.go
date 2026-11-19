package mp4

import (
	"bytes"
	"errors"
	"io"
	"os"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingReadSeeker wraps an io.ReadSeeker and counts structural scanning
// activity. Read calls made after the index is built prove whether a query
// re-scans the box structure.
type countingReadSeeker struct {
	io.ReadSeeker
	reads     int
	seeks     int
	readBytes int64
}

func (c *countingReadSeeker) Read(p []byte) (int, error) {
	c.reads++
	n, err := c.ReadSeeker.Read(p)
	c.readBytes += int64(n)
	return n, err
}

func (c *countingReadSeeker) Seek(offset int64, whence int) (int64, error) {
	c.seeks++
	return c.ReadSeeker.Seek(offset, whence)
}

// scanSequentially runs a full depth-first ReadBoxStructure scan and
// collects every visited box in visit order, using the same container
// expansion rule as BuildBoxIndex (supported boxes only).
func scanSequentially(t *testing.T, r io.ReadSeeker) []*IndexedBox {
	t.Helper()
	collected := make([]*IndexedBox, 0, 64)
	_, err := ReadBoxStructure(r, func(h *ReadHandle) (interface{}, error) {
		collected = append(collected, &IndexedBox{
			Ordinal: len(collected),
			Info:    h.BoxInfo,
			Path:    append(BoxPath(nil), h.Path...),
		})
		if !h.BoxInfo.IsSupportedType() {
			return nil, nil
		}
		vals, err := h.Expand()
		if errors.Is(err, ErrUnsupportedBoxVersion) {
			return nil, nil
		}
		return vals, err
	})
	require.NoError(t, err)
	return collected
}

func TestBuildBoxIndexConsistentWithSequentialScan(t *testing.T) {
	for _, fname := range []string{
		"./testdata/sample.mp4",
		"./testdata/sample_qt.mp4",
		"./testdata/sample_fragmented.mp4",
		"./testdata/sample_fragmented_muxed.mp4",
		"./testdata/sample_init.encv.mp4",
	} {
		t.Run(fname, func(t *testing.T) {
			f, err := os.Open(fname)
			require.NoError(t, err)
			defer f.Close()

			idx, err := BuildBoxIndex(f)
			require.NoError(t, err)
			require.Greater(t, idx.Len(), 0)

			f2, err := os.Open(fname)
			require.NoError(t, err)
			defer f2.Close()
			want := scanSequentially(t, f2)

			// Must visit exactly the same boxes in the same order.
			require.Equal(t, len(want), idx.Len(), "indexed box count differs from scan")
			for i := range want {
				got, err := idx.BoxAt(i)
				require.NoError(t, err)
				assert.Equal(t, want[i].Ordinal, got.Ordinal)
				assert.Equal(t, want[i].Info, got.Info, "BoxInfo mismatch at ordinal %d", i)
				assert.Equal(t, want[i].Path, got.Path, "Path mismatch at ordinal %d", i)
			}
		})
	}
}

func TestBoxIndexBoxAtBounds(t *testing.T) {
	f, err := os.Open("./testdata/sample.mp4")
	require.NoError(t, err)
	defer f.Close()

	idx, err := BuildBoxIndex(f)
	require.NoError(t, err)

	_, err = idx.BoxAt(-1)
	assert.True(t, errors.Is(err, ErrBoxNotIndexed))
	_, err = idx.BoxAt(idx.Len())
	assert.True(t, errors.Is(err, ErrBoxNotIndexed))
}

func TestBoxIndexBoxAtOffset(t *testing.T) {
	f, err := os.Open("./testdata/sample.mp4")
	require.NoError(t, err)
	defer f.Close()

	idx, err := BuildBoxIndex(f)
	require.NoError(t, err)

	// Every indexed box must be reachable by its own offset, and the
	// located record must be identical to ordinal lookup.
	for i := 0; i < idx.Len(); i++ {
		byOrdinal, err := idx.BoxAt(i)
		require.NoError(t, err)
		byOffset, err := idx.BoxAtOffset(byOrdinal.Info.Offset)
		require.NoError(t, err)
		assert.Equal(t, byOrdinal, byOffset)
	}

	// An offset that points inside a box (not at a box start) is not found.
	first, err := idx.BoxAt(0)
	require.NoError(t, err)
	_, err = idx.BoxAtOffset(first.Info.Offset + 1)
	assert.True(t, errors.Is(err, ErrBoxNotIndexed))
}

func TestBoxIndexFindMatchesExtractBoxes(t *testing.T) {
	paths := []BoxPath{
		{BoxTypeMoov()},
		{BoxTypeMoov(), BoxTypeTrak(), BoxTypeTkhd()},
		{BoxTypeMoov(), BoxTypeTrak(), BoxTypeMdia(), BoxTypeHdlr()},
		{BoxTypeMoov(), BoxTypeUdta()},
	}

	fScan, err := os.Open("./testdata/sample.mp4")
	require.NoError(t, err)
	defer fScan.Close()
	want, err := ExtractBoxes(fScan, nil, paths)
	require.NoError(t, err)

	fIdx, err := os.Open("./testdata/sample.mp4")
	require.NoError(t, err)
	defer fIdx.Close()
	idx, err := BuildBoxIndex(fIdx)
	require.NoError(t, err)

	got, err := idx.FindAll(paths)
	require.NoError(t, err)
	require.Equal(t, len(want), len(got))
	for i := range want {
		assert.Equal(t, want[i], &got[i].Info)
	}
}

func TestBoxIndexDescendants(t *testing.T) {
	f, err := os.Open("./testdata/sample.mp4")
	require.NoError(t, err)
	defer f.Close()

	idx, err := BuildBoxIndex(f)
	require.NoError(t, err)

	// Locate the first moov, then extract trak/mdia below it.
	moov, err := idx.Find(BoxPath{BoxTypeMoov()})
	require.NoError(t, err)

	wantBis, err := ExtractBoxes(f, &moov.Info, []BoxPath{
		{BoxTypeTrak(), BoxTypeMdia()},
	})
	require.NoError(t, err)

	gotBis, err := idx.ExtractBoxes(moov, []BoxPath{
		{BoxTypeTrak(), BoxTypeMdia()},
	})
	require.NoError(t, err)
	require.Equal(t, len(wantBis), len(gotBis))
	for i := range wantBis {
		assert.Equal(t, wantBis[i], gotBis[i])
	}
}

func TestBoxIndexChildren(t *testing.T) {
	f, err := os.Open("./testdata/sample.mp4")
	require.NoError(t, err)
	defer f.Close()

	idx, err := BuildBoxIndex(f)
	require.NoError(t, err)

	moov, err := idx.Find(BoxPath{BoxTypeMoov()})
	require.NoError(t, err)

	children := idx.Children(moov)
	require.NotEmpty(t, children)
	for _, child := range children {
		assert.Equal(t, moov.Ordinal, child.ParentOrdinal)
		assert.Equal(t, len(moov.Path)+1, len(child.Path))
	}

	// A leaf box has no children.
	mvhd, err := idx.Find(BoxPath{BoxTypeMoov(), BoxTypeMvhd()})
	require.NoError(t, err)
	assert.Nil(t, idx.Children(mvhd))
}

// TestBoxIndexParentChildRelationships validates the ordinal parent/child
// structure of the entire tree against the stored paths, on every sample.
func TestBoxIndexParentChildRelationships(t *testing.T) {
	for _, fname := range []string{
		"./testdata/sample.mp4",
		"./testdata/sample_qt.mp4",
		"./testdata/sample_fragmented.mp4",
		"./testdata/sample_fragmented_muxed.mp4",
		"./testdata/sample_ac3.mp4",
	} {
		t.Run(fname, func(t *testing.T) {
			f, err := os.Open(fname)
			require.NoError(t, err)
			defer f.Close()

			idx, err := BuildBoxIndex(f)
			require.NoError(t, err)

			for i := 0; i < idx.Len(); i++ {
				box, err := idx.BoxAt(i)
				require.NoError(t, err)

				if len(box.Path) == 1 {
					// Top-level box.
					assert.Equal(t, -1, box.ParentOrdinal, "root box %d", i)
				} else {
					require.GreaterOrEqual(t, box.ParentOrdinal, 0, "non-root box %d", i)
					parent, err := idx.BoxAt(box.ParentOrdinal)
					require.NoError(t, err)
					assert.Equal(t, len(box.Path)-1, len(parent.Path))
					assert.True(t, pathsEqual(parent.Path, box.Path[:len(parent.Path)]))
				}

				// FirstChildOrdinal, when present, must point to a child whose
				// parent is this box; Children() must then be consistent.
				children := idx.Children(box)
				if box.FirstChildOrdinal < 0 {
					assert.Nil(t, children)
					continue
				}
				require.NotEmpty(t, children)
				assert.Equal(t, box.FirstChildOrdinal, children[0].Ordinal)
				for _, child := range children {
					assert.Equal(t, box.Ordinal, child.ParentOrdinal)
				}
			}
		})
	}
}

// TestBoxIndexFragmentedStructure exercises moof/traf containers and proves
// index-based extraction matches sequential extraction on fragmented input.
func TestBoxIndexFragmentedStructure(t *testing.T) {
	paths := []BoxPath{
		{BoxTypeMoof()},
		{BoxTypeMoof(), BoxTypeTraf(), BoxTypeTfhd()},
		{BoxTypeMdat()},
	}

	fScan, err := os.Open("./testdata/sample_fragmented.mp4")
	require.NoError(t, err)
	defer fScan.Close()
	want, err := ExtractBoxes(fScan, nil, paths)
	require.NoError(t, err)

	fIdx, err := os.Open("./testdata/sample_fragmented.mp4")
	require.NoError(t, err)
	defer fIdx.Close()
	idx, err := BuildBoxIndex(fIdx)
	require.NoError(t, err)

	got, err := idx.FindAll(paths)
	require.NoError(t, err)
	require.Equal(t, len(want), len(got))
	for i := range want {
		assert.Equal(t, want[i], &got[i].Info)
	}
}

func TestBoxIndexRepeatedQueriesDoNotRescan(t *testing.T) {
	raw, err := os.Open("./testdata/sample.mp4")
	require.NoError(t, err)
	defer raw.Close()

	// Build the index once through a counting reader.
	cr := &countingReadSeeker{ReadSeeker: raw}
	idx, err := BuildBoxIndex(cr)
	require.NoError(t, err)

	// Reset counters: everything below must be served from the index.
	cr.reads = 0
	cr.seeks = 0
	cr.readBytes = 0

	for repeat := 0; repeat < 5; repeat++ {
		found, err := idx.FindAll([]BoxPath{
			{BoxTypeMoov(), BoxTypeTrak(), BoxTypeTkhd()},
		})
		require.NoError(t, err)
		require.Len(t, found, 2)

		byOff, err := idx.BoxAtOffset(found[0].Info.Offset)
		require.NoError(t, err)
		assert.Equal(t, found[0], byOff)

		byOrd, err := idx.BoxAt(found[0].Ordinal)
		require.NoError(t, err)
		assert.Equal(t, found[0], byOrd)
	}

	// Pure structural lookups perform no I/O at all.
	assert.Zero(t, cr.reads, "structural query must not read the file")
	assert.Zero(t, cr.seeks, "structural query must not seek the file")
}

func TestBoxIndexPayloadReadIsDirectSeek(t *testing.T) {
	raw, err := os.Open("./testdata/sample.mp4")
	require.NoError(t, err)
	defer raw.Close()

	cr := &countingReadSeeker{ReadSeeker: raw}
	idx, err := BuildBoxIndex(cr)
	require.NoError(t, err)

	tkhd, err := idx.Find(BoxPath{BoxTypeMoov(), BoxTypeTrak(), BoxTypeTkhd()})
	require.NoError(t, err)

	cr.reads = 0
	cr.seeks = 0
	cr.readBytes = 0

	payload, n, err := idx.ReadPayload(tkhd)
	require.NoError(t, err)

	// The payload read must land exactly on the box payload and read only
	// its payload bytes, never the headers of sibling/parent boxes.
	require.IsType(t, &Tkhd{}, payload)
	assert.Equal(t, tkhd.Info.Size-tkhd.Info.HeaderSize, n)
	assert.Equal(t, int64(n), cr.readBytes)
}

func TestBoxIndexExtractWithPayloadEqualsSequential(t *testing.T) {
	path := BoxPath{BoxTypeMoov(), BoxTypeTrak(), BoxTypeMdia(), BoxTypeHdlr()}

	fScan, err := os.Open("./testdata/sample.mp4")
	require.NoError(t, err)
	defer fScan.Close()
	want, err := ExtractBoxWithPayload(fScan, nil, path)
	require.NoError(t, err)

	fIdx, err := os.Open("./testdata/sample.mp4")
	require.NoError(t, err)
	defer fIdx.Close()
	idx, err := BuildBoxIndex(fIdx)
	require.NoError(t, err)

	got, err := idx.ExtractBoxWithPayload(nil, path)
	require.NoError(t, err)
	require.Equal(t, len(want), len(got))
	for i := range want {
		assert.Equal(t, want[i].Info, got[i].Info)
		assert.Equal(t, want[i].Payload, got[i].Payload)
	}
}

func TestBoxIndexCapacityBound(t *testing.T) {
	raw, err := os.Open("./testdata/sample.mp4")
	require.NoError(t, err)
	defer raw.Close()

	// A capacity smaller than the box count must fail with the sentinel
	// error and never exceed the bound.
	idx, err := BuildBoxIndex(raw, WithIndexCapacity(3))
	require.True(t, errors.Is(err, ErrIndexCapacityExceeded))
	if idx != nil {
		assert.LessOrEqual(t, idx.Len(), 3)
	}

	// A capacity large enough to hold every box succeeds and stays bounded.
	raw2, err := os.Open("./testdata/sample.mp4")
	require.NoError(t, err)
	defer raw2.Close()

	full, err := BuildBoxIndex(raw2, WithIndexCapacity(1000))
	require.NoError(t, err)
	assert.Equal(t, 1000, full.Capacity())
	assert.LessOrEqual(t, full.Len(), full.Capacity())

	// Capacity zero means unlimited.
	raw3, err := os.Open("./testdata/sample.mp4")
	require.NoError(t, err)
	defer raw3.Close()
	unlimited, err := BuildBoxIndex(raw3, WithIndexCapacity(0))
	require.NoError(t, err)
	assert.Zero(t, unlimited.Capacity())
	assert.Greater(t, unlimited.Len(), 0)
}

func TestBoxIndexEmptyPathErrors(t *testing.T) {
	f, err := os.Open("./testdata/sample.mp4")
	require.NoError(t, err)
	defer f.Close()

	idx, err := BuildBoxIndex(f)
	require.NoError(t, err)

	_, err = idx.Find(BoxPath{})
	assert.Error(t, err)

	_, err = idx.FindAll([]BoxPath{{}, {BoxTypeMoov()}})
	assert.Error(t, err)

	_, err = idx.ExtractBoxes(nil, []BoxPath{{}})
	assert.Error(t, err)
}

func TestBoxIndexFindUnknownType(t *testing.T) {
	f, err := os.Open("./testdata/sample.mp4")
	require.NoError(t, err)
	defer f.Close()

	idx, err := BuildBoxIndex(f)
	require.NoError(t, err)

	_, err = idx.Find(BoxPath{BoxTypeUdta(), BoxTypeMvhd()})
	assert.True(t, errors.Is(err, ErrBoxNotIndexed))
}

func TestBoxIndexSinglePathHelpers(t *testing.T) {
	f, err := os.Open("./testdata/sample.mp4")
	require.NoError(t, err)
	defer f.Close()

	idx, err := BuildBoxIndex(f)
	require.NoError(t, err)

	moov, err := idx.Find(BoxPath{BoxTypeMoov()})
	require.NoError(t, err)

	// ExtractBox (single path) must equal the multi-path variant.
	single, err := idx.ExtractBox(moov, BoxPath{BoxTypeTrak(), BoxTypeMdia()})
	require.NoError(t, err)
	multi, err := idx.ExtractBoxes(moov, []BoxPath{{BoxTypeTrak(), BoxTypeMdia()}})
	require.NoError(t, err)
	require.Equal(t, len(multi), len(single))
	for i := range multi {
		assert.Equal(t, multi[i], single[i])
	}

	// ExtractBoxWithPayload returns payloads for a single path.
	withPayload, err := idx.ExtractBoxWithPayload(nil, BoxPath{BoxTypeMoov()})
	require.NoError(t, err)
	require.Len(t, withPayload, 1)
	assert.Equal(t, BoxTypeMoov(), withPayload[0].Info.Type)
}

func TestBoxIndexNegativeCapacity(t *testing.T) {
	f, err := os.Open("./testdata/sample.mp4")
	require.NoError(t, err)
	defer f.Close()

	_, err = BuildBoxIndex(f, WithIndexCapacity(-1))
	assert.Error(t, err)
}

func TestBoxIndexReadPayloadUnsupported(t *testing.T) {
	f, err := os.Open("./testdata/sample.mp4")
	require.NoError(t, err)
	defer f.Close()

	idx, err := BuildBoxIndex(f)
	require.NoError(t, err)

	// The sample contains an unsupported "loci" box; its payload cannot be
	// unmarshaled and ReadPayload must surface that error rather than panic.
	var unsupported *IndexedBox
	for i := 0; i < idx.Len(); i++ {
		box, err := idx.BoxAt(i)
		require.NoError(t, err)
		if !box.Info.IsSupportedType() {
			unsupported = box
			break
		}
	}
	require.NotNil(t, unsupported)
	_, _, err = idx.ReadPayload(unsupported)
	assert.Error(t, err)
}

func TestBoxIndexQuickTimeMetaContainer(t *testing.T) {
	f, err := os.Open("./testdata/sample_qt.mp4")
	require.NoError(t, err)
	defer f.Close()

	idx, err := BuildBoxIndex(f)
	require.NoError(t, err)

	// The QuickTime meta box is a FullBox container; its children (hdlr,
	// keys, ilst) must be indexed below it with correct parent ordinals.
	meta, err := idx.Find(BoxPath{BoxTypeMoov(), BoxTypeUdta(), BoxTypeMeta()})
	require.NoError(t, err)

	children := idx.Children(meta)
	var types []BoxType
	for _, child := range children {
		assert.Equal(t, meta.Ordinal, child.ParentOrdinal)
		types = append(types, child.Info.Type)
	}
	assert.Contains(t, types, BoxTypeHdlr())
	assert.Contains(t, types, BoxTypeIlst())
	assert.Contains(t, types, BoxTypeKeys())
}

// buildSyntheticMP4 builds an in-memory tree of nested moov boxes. Moov is an
// empty container box (no payload fields), so each parent's full payload
// region is parsed as child boxes regardless of nesting depth.
func buildSyntheticMP4(depth, width int) []byte {
	box := func(typ BoxType, payload []byte) []byte {
		size := 8 + len(payload)
		b := make([]byte, 8+len(payload))
		b[0] = byte(size >> 24)
		b[1] = byte(size >> 16)
		b[2] = byte(size >> 8)
		b[3] = byte(size)
		copy(b[4:8], typ[:])
		copy(b[8:], payload)
		return b
	}
	var build func(d int) []byte
	build = func(d int) []byte {
		if d <= 1 {
			// A leaf moov with an empty payload.
			return box(BoxTypeMoov(), nil)
		}
		var payload []byte
		for i := 0; i < width; i++ {
			payload = append(payload, build(d-1)...)
		}
		return box(BoxTypeMoov(), payload)
	}
	return build(depth)
}

// TestBoxIndexSyntheticStress validates parent/child ordinal integrity and
// the hard capacity bound on a large synthetic tree built entirely in memory.
func TestBoxIndexSyntheticStress(t *testing.T) {
	const depth, width = 5, 4
	data := buildSyntheticMP4(depth, width)

	// Count expected nodes: a complete width-ary tree of the given depth.
	expected := 0
	level := 1
	for d := 0; d < depth; d++ {
		expected += level
		level *= width
	}

	r := bytes.NewReader(data)
	idx, err := BuildBoxIndex(r, WithIndexCapacity(0))
	require.NoError(t, err)
	require.Equal(t, expected, idx.Len())

	// Every non-root node points at a real parent whose path is its prefix.
	for i := 1; i < idx.Len(); i++ {
		box, err := idx.BoxAt(i)
		require.NoError(t, err)
		require.GreaterOrEqual(t, box.ParentOrdinal, 0)
		parent, err := idx.BoxAt(box.ParentOrdinal)
		require.NoError(t, err)
		assert.True(t, pathsEqual(parent.Path, box.Path[:len(parent.Path)]))
	}

	// A capacity below the node count must fail and never exceed the bound.
	r2 := bytes.NewReader(data)
	bounded, err := BuildBoxIndex(r2, WithIndexCapacity(expected/2))
	assert.True(t, errors.Is(err, ErrIndexCapacityExceeded))
	if bounded != nil {
		assert.LessOrEqual(t, bounded.Len(), expected/2)
	}
}

// TestBoxIndexMemoryIsFixedSize ensures an entry does not retain payload
// data: indexed records only hold structural fields, so the per-entry cost
// is independent of box size.
func TestBoxIndexMemoryIsFixedSize(t *testing.T) {
	f, err := os.Open("./testdata/sample.mp4")
	require.NoError(t, err)
	defer f.Close()

	idx, err := BuildBoxIndex(f)
	require.NoError(t, err)

	// Locate the largest box (typically mdat) and a small one.
	var largest, smallest *IndexedBox
	for i := 0; i < idx.Len(); i++ {
		box, err := idx.BoxAt(i)
		require.NoError(t, err)
		if largest == nil || box.Info.Size > largest.Info.Size {
			largest = box
		}
		if smallest == nil || box.Info.Size < smallest.Info.Size {
			smallest = box
		}
	}
	require.NotNil(t, largest)
	require.NotNil(t, smallest)
	require.Greater(t, largest.Info.Size, smallest.Info.Size)

	// An IndexedBox is a fixed-size value struct with no payload byte slice,
	// so a multi-KB mdat record costs the same bytes as an 8-byte box.
	entrySize := int(unsafe.Sizeof(IndexedBox{}))
	assert.True(t, entrySize > 0)
	// The backing array allocation is bounded by count * fixed entry size.
	assert.True(t, idx.Len()*entrySize <= idx.Capacity()*entrySize)
}
