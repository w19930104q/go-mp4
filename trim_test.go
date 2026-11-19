package mp4_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/abema/go-mp4"
)

// sampleOffset computes the data offset of every sample of the track.
func sampleOffsets(track *mp4.Track) []uint64 {
	offsets := make([]uint64, 0, len(track.Samples))
	si := 0
	for _, chunk := range track.Chunks {
		offset := chunk.DataOffset
		for i := uint32(0); i < chunk.SamplesPerChunk && si < len(track.Samples); i++ {
			offsets = append(offsets, offset)
			offset += uint64(track.Samples[si].Size)
			si++
		}
	}
	return offsets
}

// sampleTimestamps computes the decoding time of every sample of the track.
func sampleTimestamps(track *mp4.Track) []uint64 {
	timestamps := make([]uint64, 0, len(track.Samples))
	var ts uint64
	for _, sample := range track.Samples {
		timestamps = append(timestamps, ts)
		ts += uint64(sample.TimeDelta)
	}
	return timestamps
}

func readSampleData(t *testing.T, r io.ReadSeeker, offset uint64, size uint32) []byte {
	t.Helper()
	if _, err := r.Seek(int64(offset), io.SeekStart); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(r, data); err != nil {
		t.Fatal(err)
	}
	return data
}

func extractToTempFile(t *testing.T, src io.ReadSeeker, start, end uint64) *os.File {
	t.Helper()
	out, err := os.CreateTemp("", "trim-*.mp4")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		out.Close()
		os.Remove(out.Name())
	})
	if err := mp4.ExtractRange(src, out, start, end); err != nil {
		out.Close()
		os.Remove(out.Name())
		t.Fatal(err)
	}
	if _, err := out.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	return out
}

func mustProbe(t *testing.T, r io.ReadSeeker) *mp4.ProbeInfo {
	t.Helper()
	info, err := mp4.Probe(r)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

// boxPayload returns the raw bytes (header included) of the first box
// matching the path under trak.
func boxBytes(t *testing.T, r io.ReadSeeker, trak *mp4.BoxInfo, path mp4.BoxPath) []byte {
	t.Helper()
	bis, err := mp4.ExtractBox(r, trak, path)
	if err != nil {
		t.Fatal(err)
	}
	if len(bis) != 1 {
		t.Fatalf("unexpected number of boxes: %d", len(bis))
	}
	if _, err := bis[0].SeekToStart(r); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, bis[0].Size)
	if _, err := io.ReadFull(r, data); err != nil {
		t.Fatal(err)
	}
	return data
}

func trakBoxInfos(t *testing.T, r io.ReadSeeker) []*mp4.BoxInfo {
	t.Helper()
	bis, err := mp4.ExtractBox(r, nil, mp4.BoxPath{mp4.BoxTypeMoov(), mp4.BoxTypeTrak()})
	if err != nil {
		t.Fatal(err)
	}
	return bis
}

func stssOf(t *testing.T, r io.ReadSeeker, trak *mp4.BoxInfo) []uint32 {
	t.Helper()
	bips, err := mp4.ExtractBoxWithPayload(r, trak, mp4.BoxPath{
		mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStss(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(bips) == 0 {
		return nil
	}
	return bips[0].Payload.(*mp4.Stss).SampleNumber
}

func elstOf(t *testing.T, r io.ReadSeeker, trak *mp4.BoxInfo) *mp4.Elst {
	t.Helper()
	bips, err := mp4.ExtractBoxWithPayload(r, trak, mp4.BoxPath{mp4.BoxTypeEdts(), mp4.BoxTypeElst()})
	if err != nil {
		t.Fatal(err)
	}
	if len(bips) == 0 {
		return nil
	}
	return bips[0].Payload.(*mp4.Elst)
}

func mdhdOf(t *testing.T, r io.ReadSeeker, trak *mp4.BoxInfo) *mp4.Mdhd {
	t.Helper()
	bips, err := mp4.ExtractBoxWithPayload(r, trak, mp4.BoxPath{mp4.BoxTypeMdia(), mp4.BoxTypeMdhd()})
	if err != nil {
		t.Fatal(err)
	}
	if len(bips) != 1 {
		t.Fatal("mdhd box not found")
	}
	return bips[0].Payload.(*mp4.Mdhd)
}

func tkhdOf(t *testing.T, r io.ReadSeeker, trak *mp4.BoxInfo) *mp4.Tkhd {
	t.Helper()
	bips, err := mp4.ExtractBoxWithPayload(r, trak, mp4.BoxPath{mp4.BoxTypeTkhd()})
	if err != nil {
		t.Fatal(err)
	}
	if len(bips) != 1 {
		t.Fatal("tkhd box not found")
	}
	return bips[0].Payload.(*mp4.Tkhd)
}

// expectedSelection returns the indices of the samples whose decoding time
// (converted to the movie timescale) lies in [start, end).
func expectedSelection(track *mp4.Track, movieTimescale uint32, start, end uint64) (int, int) {
	timestamps := sampleTimestamps(track)
	s, e := -1, -1
	for i, ts := range timestamps {
		in := ts*uint64(movieTimescale) >= start*uint64(track.Timescale) &&
			ts*uint64(movieTimescale) < end*uint64(track.Timescale)
		if in && s < 0 {
			s = i
		}
		if in {
			e = i + 1
		}
	}
	return s, e
}

// checkExtractedTrack verifies that the extracted track contains exactly
// the source samples [s, e) with identical attributes and data bytes, and
// that the track properties are preserved.
func checkExtractedTrack(t *testing.T, srcR, outR io.ReadSeeker, srcTrack, outTrack *mp4.Track, srcTrak, outTrak *mp4.BoxInfo, s, e int) {
	t.Helper()

	// sample attributes
	if len(outTrack.Samples) != e-s {
		t.Fatalf("track %d: unexpected sample count: expected=%d actual=%d", outTrack.TrackID, e-s, len(outTrack.Samples))
	}
	for i := 0; i < e-s; i++ {
		src, out := srcTrack.Samples[s+i], outTrack.Samples[i]
		if out.Size != src.Size {
			t.Errorf("track %d: sample %d: unexpected size: expected=%d actual=%d", outTrack.TrackID, i, src.Size, out.Size)
		}
		if out.TimeDelta != src.TimeDelta {
			t.Errorf("track %d: sample %d: unexpected time delta: expected=%d actual=%d", outTrack.TrackID, i, src.TimeDelta, out.TimeDelta)
		}
		if out.CompositionTimeOffset != src.CompositionTimeOffset {
			t.Errorf("track %d: sample %d: unexpected composition time offset: expected=%d actual=%d", outTrack.TrackID, i, src.CompositionTimeOffset, out.CompositionTimeOffset)
		}
	}

	// sample data bytes
	srcOffsets := sampleOffsets(srcTrack)
	outOffsets := sampleOffsets(outTrack)
	if len(outOffsets) != e-s {
		t.Fatalf("track %d: unexpected number of sample offsets: expected=%d actual=%d", outTrack.TrackID, e-s, len(outOffsets))
	}
	for i := 0; i < e-s; i++ {
		srcData := readSampleData(t, srcR, srcOffsets[s+i], srcTrack.Samples[s+i].Size)
		outData := readSampleData(t, outR, outOffsets[i], outTrack.Samples[i].Size)
		if !bytes.Equal(srcData, outData) {
			t.Errorf("track %d: sample %d: data mismatch", outTrack.TrackID, i)
		}
	}

	// track properties
	if outTrack.TrackID != srcTrack.TrackID {
		t.Errorf("unexpected track ID: expected=%d actual=%d", srcTrack.TrackID, outTrack.TrackID)
	}
	if outTrack.Timescale != srcTrack.Timescale {
		t.Errorf("track %d: unexpected timescale: expected=%d actual=%d", outTrack.TrackID, srcTrack.Timescale, outTrack.Timescale)
	}
	stsdPath := mp4.BoxPath{mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl(), mp4.BoxTypeStsd()}
	if !bytes.Equal(boxBytes(t, srcR, srcTrak, stsdPath), boxBytes(t, outR, outTrak, stsdPath)) {
		t.Errorf("track %d: stsd mismatch", outTrack.TrackID)
	}
	hdlrPath := mp4.BoxPath{mp4.BoxTypeMdia(), mp4.BoxTypeHdlr()}
	if !bytes.Equal(boxBytes(t, srcR, srcTrak, hdlrPath), boxBytes(t, outR, outTrak, hdlrPath)) {
		t.Errorf("track %d: hdlr mismatch", outTrack.TrackID)
	}
	srcTkhd, outTkhd := tkhdOf(t, srcR, srcTrak), tkhdOf(t, outR, outTrak)
	if outTkhd.Width != srcTkhd.Width || outTkhd.Height != srcTkhd.Height {
		t.Errorf("track %d: unexpected dimension: expected=%dx%d actual=%dx%d",
			outTrack.TrackID, srcTkhd.Width, srcTkhd.Height, outTkhd.Width, outTkhd.Height)
	}

	// duration of the extracted part
	var duration uint64
	for _, sample := range outTrack.Samples {
		duration += uint64(sample.TimeDelta)
	}
	if mdhd := mdhdOf(t, outR, outTrak); mdhd.GetDuration() != duration {
		t.Errorf("track %d: unexpected mdhd duration: expected=%d actual=%d", outTrack.TrackID, duration, mdhd.GetDuration())
	}
}

func openTestData(t *testing.T, name string) *os.File {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestExtractRangeMiddle(t *testing.T) {
	src := openTestData(t, "sample.mp4")
	srcInfo := mustProbe(t, src)
	if len(srcInfo.Tracks) != 2 {
		t.Fatalf("unexpected number of tracks: %d", len(srcInfo.Tracks))
	}

	const start, end = 500, 700 // [0.5s, 0.7s) in the movie timescale (1000)
	out := extractToTempFile(t, src, start, end)
	outInfo := mustProbe(t, out)

	if len(outInfo.Tracks) != len(srcInfo.Tracks) {
		t.Fatalf("unexpected number of tracks: expected=%d actual=%d", len(srcInfo.Tracks), len(outInfo.Tracks))
	}

	srcTraks := trakBoxInfos(t, src)
	outTraks := trakBoxInfos(t, out)

	total := 0
	for i := range srcInfo.Tracks {
		srcTrack, outTrack := srcInfo.Tracks[i], outInfo.Tracks[i]
		s, e := expectedSelection(srcTrack, srcInfo.Timescale, start, end)
		if s < 0 {
			t.Fatalf("track %d: no sample expected in the range", srcTrack.TrackID)
		}
		total += e - s
		checkExtractedTrack(t, src, out, srcTrack, outTrack, srcTraks[i], outTraks[i], s, e)
	}
	if total == 0 {
		t.Fatal("no sample selected")
	}

	// the video track (trackID=1) must contain exactly the samples 5 and 6
	video := outInfo.Tracks[0]
	if video.TrackID != 1 {
		t.Fatalf("unexpected track order: trackID=%d", video.TrackID)
	}
	if len(video.Samples) != 2 {
		t.Fatalf("unexpected video sample count: %d", len(video.Samples))
	}
	srcSizes := []uint32{srcInfo.Tracks[0].Samples[5].Size, srcInfo.Tracks[0].Samples[6].Size}
	for i, sample := range video.Samples {
		if sample.Size != srcSizes[i] {
			t.Errorf("unexpected video sample size: index=%d expected=%d actual=%d", i, srcSizes[i], sample.Size)
		}
	}

	// no sync sample in [500, 700): the stss box must be empty
	if stss := stssOf(t, out, outTraks[0]); len(stss) != 0 {
		t.Errorf("unexpected stss: %v", stss)
	}

	// the whole output must be a well-formed MP4
	if _, err := mp4.ReadBoxStructure(out, func(*mp4.ReadHandle) (interface{}, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
}

func TestExtractRangeFull(t *testing.T) {
	src := openTestData(t, "sample.mp4")
	srcInfo := mustProbe(t, src)

	out := extractToTempFile(t, src, 0, srcInfo.Duration)
	outInfo := mustProbe(t, out)

	srcTraks := trakBoxInfos(t, src)
	outTraks := trakBoxInfos(t, out)

	for i := range srcInfo.Tracks {
		srcTrack, outTrack := srcInfo.Tracks[i], outInfo.Tracks[i]
		checkExtractedTrack(t, src, out, srcTrack, outTrack, srcTraks[i], outTraks[i], 0, len(srcTrack.Samples))

		// the edit list media time of the source must be preserved
		srcElst, outElst := elstOf(t, src, srcTraks[i]), elstOf(t, out, outTraks[i])
		if srcElst == nil || outElst == nil {
			t.Fatalf("track %d: edit list not found", srcTrack.TrackID)
		}
		if outElst.GetMediaTime(0) != srcElst.GetMediaTime(0) {
			t.Errorf("track %d: unexpected elst media time: expected=%d actual=%d",
				srcTrack.TrackID, srcElst.GetMediaTime(0), outElst.GetMediaTime(0))
		}
	}

	// the sync sample of the video track must be kept
	if stss := stssOf(t, out, outTraks[0]); len(stss) != 1 || stss[0] != 1 {
		t.Errorf("unexpected stss: %v", stss)
	}

	// durations must be identical to the source
	if outInfo.Duration != srcInfo.Duration {
		t.Errorf("unexpected mvhd duration: expected=%d actual=%d", srcInfo.Duration, outInfo.Duration)
	}
	for i := range srcInfo.Tracks {
		if outInfo.Tracks[i].Duration != srcInfo.Tracks[i].Duration {
			t.Errorf("track %d: unexpected duration: expected=%d actual=%d",
				i, srcInfo.Tracks[i].Duration, outInfo.Tracks[i].Duration)
		}
	}
}

func TestExtractRangeBoundaries(t *testing.T) {
	src := openTestData(t, "sample.mp4")
	srcInfo := mustProbe(t, src)

	testCases := []struct {
		name       string
		start, end uint64
	}{
		// the first video sample only; the second sample starts exactly at
		// the end time and must be excluded
		{name: "first sample", start: 0, end: 100},
		// start and end exactly on video sample boundaries
		{name: "on boundaries", start: 100, end: 300},
		// the last video sample; the end is the track end
		{name: "last sample", start: 900, end: 1000},
		// the whole media
		{name: "whole", start: 0, end: 1024},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := src.Seek(0, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			out := extractToTempFile(t, src, tc.start, tc.end)
			outInfo := mustProbe(t, out)

			srcTraks := trakBoxInfos(t, src)
			outTraks := trakBoxInfos(t, out)
			for i := range srcInfo.Tracks {
				s, e := expectedSelection(srcInfo.Tracks[i], srcInfo.Timescale, tc.start, tc.end)
				if s < 0 {
					t.Fatalf("track %d: no sample expected in the range", srcInfo.Tracks[i].TrackID)
				}
				checkExtractedTrack(t, src, out, srcInfo.Tracks[i], outInfo.Tracks[i], srcTraks[i], outTraks[i], s, e)
			}
		})
	}
}

func TestExtractRangeBoundaryExactness(t *testing.T) {
	src := openTestData(t, "sample.mp4")
	srcInfo := mustProbe(t, src)
	video := srcInfo.Tracks[0]

	// video sample i starts exactly at i*100 ticks (timescale 10240, delta 1024)
	out := extractToTempFile(t, src, 100, 300)
	outInfo := mustProbe(t, out)
	if len(outInfo.Tracks[0].Samples) != 2 {
		t.Fatalf("unexpected sample count: %d", len(outInfo.Tracks[0].Samples))
	}
	// the extracted samples must be the source samples 1 and 2
	for i, sample := range outInfo.Tracks[0].Samples {
		if sample.Size != video.Samples[i+1].Size {
			t.Errorf("unexpected sample: index=%d", i)
		}
	}
}

func TestExtractRangeOutOfRange(t *testing.T) {
	testCases := []struct {
		name       string
		start, end uint64
		outOfRange bool
	}{
		{name: "empty range", start: 500, end: 500},
		{name: "inverted range", start: 700, end: 500},
		{name: "end exceeds duration", start: 0, end: 1025, outOfRange: true},
		{name: "far beyond duration", start: 2000, end: 3000, outOfRange: true},
		// [1000, 1024) contains audio samples but no video sample
		{name: "no video sample", start: 1000, end: 1024, outOfRange: true},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			src := openTestData(t, "sample.mp4")
			out, err := os.CreateTemp("", "trim-*.mp4")
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				out.Close()
				os.Remove(out.Name())
			}()
			err = mp4.ExtractRange(src, out, tc.start, tc.end)
			if err == nil {
				t.Fatal("expected an error")
			}
			if tc.outOfRange && !errors.Is(err, mp4.ErrOutOfRange) {
				t.Fatalf("expected ErrOutOfRange, got: %v", err)
			}
		})
	}
}

func TestExtractRangeRoundTrip(t *testing.T) {
	src := openTestData(t, "sample.mp4")
	srcInfo := mustProbe(t, src)

	// [500, 700) of the source is [0, 200) of the extracted file
	mid := extractToTempFile(t, src, 500, 700)
	midInfo := mustProbe(t, mid)
	out := extractToTempFile(t, mid, 0, 200)
	outInfo := mustProbe(t, out)

	midTraks := trakBoxInfos(t, mid)
	outTraks := trakBoxInfos(t, out)
	for i := range midInfo.Tracks {
		checkExtractedTrack(t, mid, out, midInfo.Tracks[i], outInfo.Tracks[i], midTraks[i], outTraks[i], 0, len(midInfo.Tracks[i].Samples))
	}

	// and the samples must still be identical to the source ones
	srcTraks := trakBoxInfos(t, src)
	for i := range srcInfo.Tracks {
		s, e := expectedSelection(srcInfo.Tracks[i], srcInfo.Timescale, 500, 700)
		checkExtractedTrack(t, src, out, srcInfo.Tracks[i], outInfo.Tracks[i], srcTraks[i], outTraks[i], s, e)
	}
}

func TestExtractRangeSingleTrack(t *testing.T) {
	src := openTestData(t, "sample_ac3.mp4")
	srcInfo := mustProbe(t, src)
	if len(srcInfo.Tracks) != 1 {
		t.Fatalf("unexpected number of tracks: %d", len(srcInfo.Tracks))
	}

	start, end := srcInfo.Duration/4, srcInfo.Duration/2
	out := extractToTempFile(t, src, start, end)
	outInfo := mustProbe(t, out)

	if len(outInfo.Tracks) != 1 {
		t.Fatalf("unexpected number of tracks: %d", len(outInfo.Tracks))
	}
	s, e := expectedSelection(srcInfo.Tracks[0], srcInfo.Timescale, start, end)
	if s < 0 {
		t.Fatal("no sample expected in the range")
	}
	checkExtractedTrack(t, src, out, srcInfo.Tracks[0], outInfo.Tracks[0], trakBoxInfos(t, src)[0], trakBoxInfos(t, out)[0], s, e)
}

func TestExtractRangeFragmented(t *testing.T) {
	src := openTestData(t, "sample_fragmented.mp4")
	out, err := os.CreateTemp("", "trim-*.mp4")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		out.Close()
		os.Remove(out.Name())
	}()
	if err := mp4.ExtractRange(src, out, 0, 1000); err == nil {
		t.Fatal("expected an error for a fragmented MP4")
	}
}
