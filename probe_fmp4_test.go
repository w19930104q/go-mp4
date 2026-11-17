package mp4

import (
	"bytes"
	"encoding/binary"
	"io"
	"math"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func concat(parts ...[]byte) []byte {
	var buf bytes.Buffer
	for _, p := range parts {
		buf.Write(p)
	}
	return buf.Bytes()
}

func u32(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }

func u64(v uint64) []byte { return binary.BigEndian.AppendUint64(nil, v) }

func u16(v uint16) []byte { return binary.BigEndian.AppendUint16(nil, v) }

// rawBox wraps a payload with an 8-byte box header.
func rawBox(boxType BoxType, payload []byte) []byte {
	buf := make([]byte, SmallHeaderSize+len(payload))
	binary.BigEndian.PutUint32(buf, uint32(SmallHeaderSize+len(payload)))
	copy(buf[4:8], boxType[:])
	copy(buf[8:], payload)
	return buf
}

func rawFullBox(boxType BoxType, version uint8, flags uint32, payload []byte) []byte {
	header := []byte{version, byte(flags >> 16), byte(flags >> 8), byte(flags)}
	return rawBox(boxType, append(header, payload...))
}

// buildFMP4Fixtures returns the fixed fragmented-MP4 variants used by the
// probe tests. Every variant is derived from the same synthetic fragment
// layout so the resulting segment tables can be diffed field by field.
func buildFMP4Fixtures(t *testing.T) map[string][]byte {
	t.Helper()

	// minimal ftyp
	ftyp := rawBox(BoxTypeFtyp(), concat([]byte("isom"), u32(0), []byte("isomiso2")))

	// moov/mvex/trex: track 1 with default sample duration/size.
	trex := rawFullBox(BoxTypeTrex(), 0, 0, concat(u32(1), u32(1), u32(1000), u32(10), u32(0)))
	mvex := rawBox(BoxTypeMvex(), trex)
	moov := rawBox(BoxTypeMoov(), mvex)

	// one moof per fragment. tfhd carries default sample duration/size,
	// tfdt gives the base media decode time, trun carries per-sample fields.
	buildMoof := func(seq, baseTime uint32, durations, sizes []uint32, cttos []int32) []byte {
		mfhd := rawFullBox(BoxTypeMfhd(), 0, 0, u32(seq))

		tfhdFlags := uint32(0x000008 | 0x000010) // default-sample-duration and -size
		tfhd := rawFullBox(BoxTypeTfhd(), 0, tfhdFlags, concat(u32(1), u32(1000), u32(10)))

		tfdt := rawFullBox(BoxTypeTfdt(), 0, 0, u32(baseTime))

		var trunFlags uint32 = 0x000100 | 0x000200
		withCtto := len(cttos) > 0
		if withCtto {
			trunFlags |= 0x000800
		}
		trunPayload := u32(uint32(len(durations)))
		for i := range durations {
			trunPayload = append(trunPayload, u32(durations[i])...)
			trunPayload = append(trunPayload, u32(sizes[i])...)
			if withCtto {
				trunPayload = append(trunPayload, u32(uint32(cttos[i]))...)
			}
		}
		trun := rawFullBox(BoxTypeTrun(), 0, trunFlags, trunPayload)

		traf := rawBox(BoxTypeTraf(), concat(tfhd, tfdt, trun))
		return rawBox(BoxTypeMoof(), concat(mfhd, traf))
	}
	buildMdat := func(payloadSize int) []byte {
		return rawBox(BoxTypeMdat(), bytes.Repeat([]byte{0xaa}, payloadSize))
	}

	moof0 := buildMoof(1, 0, []uint32{1000, 1000, 1000}, []uint32{10, 20, 30}, []int32{2000, 1000, 0})
	mdat0 := buildMdat(60)
	moof1 := buildMoof(2, 3000, []uint32{1000, 1000}, []uint32{40, 50}, nil)
	mdat1 := buildMdat(90)
	media := concat(moof0, mdat0, moof1, mdat1)

	// header-only styp (valid box with empty payload)
	emptyStyp := rawBox(BoxTypeStyp(), nil)

	// sidx with two media references
	buildSidx := func(version uint8, firstOffset uint64, ept uint64) []byte {
		payload := concat(u32(1), u32(90000)) // reference_ID, timescale
		if version == 1 {
			payload = append(payload, u64(ept)...)
			payload = append(payload, u64(firstOffset)...)
		} else {
			payload = append(payload, u32(uint32(ept))...)
			payload = append(payload, u32(uint32(firstOffset))...)
		}
		payload = append(payload, u16(0)...) // reserved
		payload = append(payload, u16(2)...) // reference count
		ref := func(size, duration uint32) []byte {
			return concat(
				u32(size&0x7fffffff), // media reference (reference_type bit = 0)
				u32(duration),
				u32(0xc0000000), // starts-with-SAP, SAP type 1, delta 0
			)
		}
		payload = append(payload, ref(uint32(len(moof0)+len(mdat0)), 3000)...)
		payload = append(payload, ref(uint32(len(moof1)+len(mdat1)), 2000)...)
		return rawFullBox(BoxTypeSidx(), version, 0, payload)
	}
	sidxV1 := buildSidx(1, 0, 0)
	sidxV1BigOffset := buildSidx(1, 0x1_0000_1000, 5)
	sidxV0 := buildSidx(0, 0, 0)

	// mfra with two tfra entries plus mfro, duplicated for the consecutive case
	tfra := func(trackID uint32) []byte {
		return rawFullBox(BoxTypeTfra(), 0, 0, concat(
			u32(trackID),
			u32(0), // reserved + length sizes = 0
			u32(1), // one entry
			u32(0), // time v0
			u32(0), // moof offset v0
			u32(1), // traf number
			u32(1), // trun number
			u32(1), // sample number
		))
	}
	mfro := rawFullBox(BoxTypeMfro(), 0, 0, u32(0))
	mfra := rawBox(BoxTypeMfra(), concat(tfra(1), tfra(2), mfro))

	uuidBox := rawBox(StrToBoxType("uuid"), bytes.Repeat([]byte{0x01}, 16))
	unknownBox := rawBox(StrToBoxType("zzzz"), []byte{1, 2, 3, 4})

	return map[string][]byte{
		"baseline":          concat(ftyp, moov, media),
		"empty_styp":        concat(ftyp, moov, emptyStyp, media),
		"sidx_v1":           concat(ftyp, moov, sidxV1, media, mfra),
		"sidx_v1_bigoffset": concat(ftyp, moov, sidxV1BigOffset, media),
		"sidx_v0":           concat(ftyp, moov, sidxV0, media),
		"double_mfra":       concat(ftyp, moov, media, mfra, mfra),
		"unknown_boxes":     concat(ftyp, uuidBox, moov, unknownBox, media, mfra),
		"combined":          concat(ftyp, uuidBox, moov, sidxV1, unknownBox, emptyStyp, media, mfra, mfra),
	}
}

func probeBytes(t *testing.T, data []byte) (*ProbeInfo, error) {
	t.Helper()
	return Probe(bytes.NewReader(data))
}

// assertSameSegmentTable diffs the segment table of a variant against the
// baseline. Offsets naturally shift when boxes are inserted, so they are
// excluded from the field comparison.
func assertSameSegmentTable(t *testing.T, expected, actual *ProbeInfo) {
	t.Helper()
	require.Len(t, actual.Segments, len(expected.Segments))
	for i := range expected.Segments {
		e, a := expected.Segments[i], actual.Segments[i]
		assert.Equal(t, e.TrackID, a.TrackID, "segment %d TrackID", i)
		assert.Equal(t, e.BaseMediaDecodeTime, a.BaseMediaDecodeTime, "segment %d BaseMediaDecodeTime", i)
		assert.Equal(t, e.DefaultSampleDuration, a.DefaultSampleDuration, "segment %d DefaultSampleDuration", i)
		assert.Equal(t, e.SampleCount, a.SampleCount, "segment %d SampleCount", i)
		assert.Equal(t, e.Duration, a.Duration, "segment %d Duration", i)
		assert.Equal(t, e.Size, a.Size, "segment %d Size", i)
		assert.Equal(t, e.CompositionTimeOffset, a.CompositionTimeOffset, "segment %d CompositionTimeOffset", i)
	}
}

func TestProbeFMP4VariantsSegmentTable(t *testing.T) {
	fixtures := buildFMP4Fixtures(t)
	baseline, err := probeBytes(t, fixtures["baseline"])
	require.NoError(t, err)
	require.Len(t, baseline.Segments, 2)

	// baseline sanity checks
	assert.Equal(t, uint32(1), baseline.Segments[0].TrackID)
	assert.Equal(t, uint64(0), baseline.Segments[0].BaseMediaDecodeTime)
	assert.Equal(t, uint32(3), baseline.Segments[0].SampleCount)
	assert.Equal(t, uint32(3000), baseline.Segments[0].Duration)
	assert.Equal(t, uint32(60), baseline.Segments[0].Size)
	// each sample's running-duration + CTTO is >= 2000, so the earliest
	// composition offset is 2000 (matching the legacy probe behavior)
	assert.Equal(t, int32(2000), baseline.Segments[0].CompositionTimeOffset)
	assert.Equal(t, uint64(3000), baseline.Segments[1].BaseMediaDecodeTime)
	assert.Equal(t, uint32(2), baseline.Segments[1].SampleCount)
	assert.Equal(t, uint32(2000), baseline.Segments[1].Duration)
	assert.Equal(t, uint32(90), baseline.Segments[1].Size)
	assert.Len(t, baseline.SegmentIndexes, 0)

	for _, name := range []string{
		"empty_styp",
		"sidx_v1",
		"sidx_v1_bigoffset",
		"double_mfra",
		"unknown_boxes",
		"combined",
		"sidx_v0",
	} {
		t.Run(name, func(t *testing.T) {
			info, err := probeBytes(t, fixtures[name])
			require.NoError(t, err)
			assertSameSegmentTable(t, baseline, info)
		})
	}
}

func TestProbeSidxSummary(t *testing.T) {
	fixtures := buildFMP4Fixtures(t)

	t.Run("version1 64bit", func(t *testing.T) {
		info, err := probeBytes(t, fixtures["sidx_v1"])
		require.NoError(t, err)
		require.Len(t, info.SegmentIndexes, 1)
		sx := info.SegmentIndexes[0]
		assert.Equal(t, uint8(1), sx.Version)
		assert.Equal(t, uint32(1), sx.ReferenceID)
		assert.Equal(t, uint32(90000), sx.Timescale)
		assert.Equal(t, uint64(0), sx.FirstOffset)
		require.Len(t, sx.References, 2)
		assert.False(t, sx.References[0].ReferencedType)
		assert.True(t, sx.References[0].StartsWithSAP)
		assert.Equal(t, uint32(3000), sx.References[0].SubsegmentDuration)
		assert.Equal(t, uint32(2000), sx.References[1].SubsegmentDuration)
	})

	t.Run("version1 large offset", func(t *testing.T) {
		info, err := probeBytes(t, fixtures["sidx_v1_bigoffset"])
		require.NoError(t, err)
		require.Len(t, info.SegmentIndexes, 1)
		// the >32bit offset must survive normalization untouched
		assert.Equal(t, uint64(0x1_0000_1000), info.SegmentIndexes[0].FirstOffset)
		assert.Equal(t, uint64(5), info.SegmentIndexes[0].EarliestPresentationTime)
	})

	t.Run("version0 32bit", func(t *testing.T) {
		info, err := probeBytes(t, fixtures["sidx_v0"])
		require.NoError(t, err)
		require.Len(t, info.SegmentIndexes, 1)
		sx := info.SegmentIndexes[0]
		assert.Equal(t, uint8(0), sx.Version)
		assert.Equal(t, uint64(0), sx.FirstOffset)
		require.Len(t, sx.References, 2)
	})

	t.Run("multiple sidx boxes", func(t *testing.T) {
		// two consecutive sidx boxes are both summarized and kept in order
		sidxBox := extractFirstBox(t, fixtures["sidx_v1"], BoxTypeSidx())
		moovEnd := boxEnd(t, fixtures["sidx_v1"], BoxTypeMoov())
		rebuilt := append([]byte{}, fixtures["sidx_v1"][:moovEnd]...)
		rebuilt = append(rebuilt, sidxBox...)
		rebuilt = append(rebuilt, sidxBox...)
		rebuilt = append(rebuilt, fixtures["sidx_v1"][moovEnd+len(sidxBox):]...)

		info, err := probeBytes(t, rebuilt)
		require.NoError(t, err)
		require.Len(t, info.SegmentIndexes, 2)
		assert.Equal(t, info.SegmentIndexes[0].Offset+uint64(len(sidxBox)), info.SegmentIndexes[1].Offset)
	})
}

func TestProbeEmptyStyp(t *testing.T) {
	// a header-only styp (valid box, empty payload) interspersed between the
	// initialization segment and the media must not make Probe fail.
	fixtures := buildFMP4Fixtures(t)
	info, err := probeBytes(t, fixtures["empty_styp"])
	require.NoError(t, err)
	require.Len(t, info.Segments, 2)
	require.Len(t, info.Tracks, 0) // synthetic fixtures carry no trak boxes
}

func TestProbeTruncatedFilesReturnErrors(t *testing.T) {
	fixtures := buildFMP4Fixtures(t)
	for name, data := range fixtures {
		data := data
		// cut at every byte length from 1 up to len-1; Probe must never
		// panic on a truncated prefix.
		for cut := 1; cut < len(data); cut++ {
			cut := cut
			t.Run(name, func(t *testing.T) {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("panic at cut=%d: %v", cut, r)
					}
				}()
				info, err := Probe(bytes.NewReader(data[:cut]))
				if err != nil {
					// any truncated box must surface as an explicit error
					assert.Nil(t, info)
					return
				}
				// No error is only possible when the cut lands in a gap
				// between complete boxes; every segment the result reports
				// must then be fully contained in the prefix.
				require.NotNil(t, info)
				for _, seg := range info.Segments {
					assert.LessOrEqual(t, seg.MoofOffset, uint64(cut))
				}
			})
		}
	}
}

// TestProbeTruncatedBoxDeclaresError verifies the specific requirement:
// a box whose declared size extends past EOF yields an error instead of a
// partial result or a panic, for every variant and every box position.
func TestProbeTruncatedBoxDeclaresError(t *testing.T) {
	fixtures := buildFMP4Fixtures(t)
	for name, data := range fixtures {
		data := data
		boxes := scanTopBoxes(t, data)
		require.NotEmpty(t, boxes)
		last := boxes[len(boxes)-1]
		// truncate one byte inside the final declared box
		cut := last.offset + last.size - 1
		require.Less(t, cut, len(data))
		t.Run(name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic: %v", r)
				}
			}()
			info, err := Probe(bytes.NewReader(data[:cut]))
			require.Error(t, err)
			assert.Nil(t, info)
		})
	}
}

// TestProbeTruncatedNestedBox verifies the recursive header scan catches a
// truncation inside a nested container (moof > traf > trun) even though the
// enclosing moof header itself is fully readable.
func TestProbeTruncatedNestedBox(t *testing.T) {
	fixtures := buildFMP4Fixtures(t)
	moof := extractFirstBox(t, fixtures["baseline"], BoxTypeMoof())
	require.Greater(t, len(moof), SmallHeaderSize)

	// cut inside moof but after its own header, dropping part of the children
	cut := len(fixtures["baseline"]) - len(moof) + SmallHeaderSize + 1
	partial := fixtures["baseline"][:cut]

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic: %v", r)
		}
	}()
	info, err := Probe(bytes.NewReader(partial))
	require.Error(t, err)
	assert.Nil(t, info)
}

func TestProbeTruncatedKnownFiles(t *testing.T) {
	// the real fragmented sample must also never panic when truncated
	f, err := os.Open("./testdata/sample_fragmented.mp4")
	require.NoError(t, err)
	data, err := io.ReadAll(f)
	f.Close()
	require.NoError(t, err)

	// determine which cuts fall strictly inside a declared top-level box
	var layout []*BoxInfo
	_, err = ReadBoxStructure(bytes.NewReader(data), func(h *ReadHandle) (interface{}, error) {
		layout = append(layout, &h.BoxInfo)
		return nil, nil
	})
	require.NoError(t, err)
	insideDeclaredBox := func(cut int) bool {
		for _, b := range layout {
			if uint64(cut) > b.Offset && uint64(cut) < b.Offset+b.Size {
				return true
			}
		}
		return false
	}

	for _, cut := range []int{1, 7, 8, 31, 32, 33, 100, 1227, 1300, len(data) - 16, len(data) - 1} {
		cut := cut
		t.Run("cut", func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic at cut=%d: %v", cut, r)
				}
			}()
			info, err := Probe(bytes.NewReader(data[:cut]))
			// A cut shorter than one box header keeps the historical "no
			// boxes" semantics; once a full header is readable, truncation
			// inside a declared box must surface as an error.
			if cut >= SmallHeaderSize && insideDeclaredBox(cut) {
				require.Error(t, err, "cut=%d", cut)
				assert.Nil(t, info)
			}
		})
	}
}

func TestProbeSegmentDurationOverflow(t *testing.T) {
	// build a moof whose trun durations sum past uint32
	mfhd := rawFullBox(BoxTypeMfhd(), 0, 0, u32(1))
	tfhd := rawFullBox(BoxTypeTfhd(), 0, 0x000008, concat(u32(1), u32(1000)))
	tfdt := rawFullBox(BoxTypeTfdt(), 0, 0, u32(0))
	trunPayload := concat(
		u32(2),
		u32(math.MaxUint32), u32(10),
		u32(math.MaxUint32), u32(10),
	)
	trun := rawFullBox(BoxTypeTrun(), 0, 0x000100|0x000200, trunPayload)
	moof := rawBox(BoxTypeMoof(), concat(mfhd, rawBox(BoxTypeTraf(), concat(tfhd, tfdt, trun))))

	ftyp := rawBox(BoxTypeFtyp(), concat([]byte("isom"), u32(0), []byte("isomiso2")))
	trex := rawFullBox(BoxTypeTrex(), 0, 0, concat(u32(1), u32(1), u32(1000), u32(10), u32(0)))
	moov := rawBox(BoxTypeMoov(), rawBox(BoxTypeMvex(), trex))

	info, err := Probe(bytes.NewReader(concat(ftyp, moov, moof)))
	require.Error(t, err)
	assert.Nil(t, info)
	assert.Contains(t, err.Error(), "overflows uint32")
}

// scanTopBoxes returns (type, offset, size) for each top-level box.
func scanTopBoxes(t *testing.T, data []byte) []struct {
	boxType BoxType
	offset  int
	size    int
} {
	t.Helper()
	var out []struct {
		boxType BoxType
		offset  int
		size    int
	}
	for off := 0; off+8 <= len(data); {
		size := int(binary.BigEndian.Uint32(data[off : off+4]))
		var boxType BoxType
		copy(boxType[:], data[off+4:off+8])
		require.Greater(t, size, 0)
		out = append(out, struct {
			boxType BoxType
			offset  int
			size    int
		}{boxType, off, size})
		off += size
	}
	return out
}

func extractFirstBox(t *testing.T, data []byte, target BoxType) []byte {
	t.Helper()
	for _, b := range scanTopBoxes(t, data) {
		if b.boxType == target {
			return data[b.offset : b.offset+b.size]
		}
	}
	t.Fatalf("box %s not found", target.String())
	return nil
}

func boxEnd(t *testing.T, data []byte, target BoxType) int {
	t.Helper()
	for _, b := range scanTopBoxes(t, data) {
		if b.boxType == target {
			return b.offset + b.size
		}
	}
	t.Fatalf("box %s not found", target.String())
	return 0
}
