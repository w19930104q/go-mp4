package mp4

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Reference values were cross-checked with ffprobe, whose output reflects the
// ISO 14496-12 edit-list semantics implemented by common players.
//
// All generated files derive from testdata/sample.mp4:
//   - movie timescale (mvhd): 1000
//   - video track: media timescale 10240, 10 samples of delta 1024
//   - audio track: media timescale 44100

func probeFile(t *testing.T, name string) *ProbeInfo {
	t.Helper()
	f, err := os.Open("./testdata/" + name)
	require.NoError(t, err)
	defer f.Close()
	info, err := Probe(f)
	require.NoError(t, err)
	return info
}

func TestProbeEditListSingleEntry(t *testing.T) {
	// sample.mp4: one non-empty edit per track.
	info := probeFile(t, "sample.mp4")
	require.Equal(t, uint32(1000), info.Timescale)

	video := info.Tracks[0]
	assert.Equal(t, uint32(10240), video.Timescale)
	assert.Equal(t, uint32(1000), video.MovieTimescale)
	require.Len(t, video.EditList, 1)
	assert.False(t, video.EditList[0].IsEmpty())

	// the presentation duration is the edit-list segment duration (movie units),
	// matching the 1.0 s reported by players instead of raw mdhd math.
	assert.Equal(t, uint64(1000), video.GetPresentationDuration())
	assert.Equal(t, uint64(1000), video.EditList.GetPresentationDuration())

	audio := info.Tracks[1]
	assert.Equal(t, uint32(44100), audio.Timescale)
	require.Len(t, audio.EditList, 1)
	assert.Equal(t, uint64(1000), audio.GetPresentationDuration())
}

func TestProbeEditListEmptyEdit(t *testing.T) {
	// video track: empty edit of 200 ms + real edit of 1000 ms.
	info := probeFile(t, "sample_empty_edit.mp4")
	require.Equal(t, uint32(1000), info.Timescale)

	video := info.Tracks[0]
	require.Len(t, video.EditList, 2)
	assert.True(t, video.EditList[0].IsEmpty())
	assert.False(t, video.EditList[1].IsEmpty())

	// empty edits count toward the presentation duration (ffprobe: 1.2 s)...
	assert.Equal(t, uint64(1200), video.GetPresentationDuration())
	// ...but consume no media: the first entry alone contributes zero
	assert.Equal(t, uint64(0), EditList{video.EditList[0]}.GetMediaDuration(info.Timescale, video.Timescale))
	assert.Equal(t, uint64(10240), video.EditList.GetMediaDuration(info.Timescale, video.Timescale))

	// presentation time inside the empty edit cannot map to media
	_, ok := video.GetMediaTime(100)
	assert.False(t, ok)

	// media at the edit's media_time starts after the empty interval
	pt, ok := video.GetMediaTime(200)
	require.True(t, ok)
	assert.Equal(t, uint64(2048), pt)

	// inverse direction: media time 2048 maps to presentation time 200
	got, ok := video.GetPresentationTime(2048)
	require.True(t, ok)
	assert.Equal(t, uint64(200), got)
}

func TestProbeEditListMultipleSegments(t *testing.T) {
	// video track: [0.3 s -> media@2048] + [0.4 s -> media@6144]
	info := probeFile(t, "sample_multi_edit.mp4")
	video := info.Tracks[0]
	require.Len(t, video.EditList, 2)
	for _, e := range video.EditList {
		assert.False(t, e.IsEmpty())
	}
	// ffprobe reports a 0.7 s video stream.
	assert.Equal(t, uint64(700), video.GetPresentationDuration())
	// both segments consume media: (300 + 400) ms * 10240 / 1000 = 7168
	assert.Equal(t, uint64(7168), video.EditList.GetMediaDuration(info.Timescale, video.Timescale))

	// first segment: presentation 0..300 maps to media 2048..
	mt, ok := video.GetMediaTime(0)
	require.True(t, ok)
	assert.Equal(t, uint64(2048), mt)
	// second segment starts at presentation 300 and maps to media 6144
	mt, ok = video.GetMediaTime(300)
	require.True(t, ok)
	assert.Equal(t, uint64(6144), mt)

	// inverse mapping across the cut: media 6144 is presented at 300
	pt, ok := video.GetPresentationTime(6144)
	require.True(t, ok)
	assert.Equal(t, uint64(300), pt)
	// media inside the first segment is presented
	pt, ok = video.GetPresentationTime(4096)
	require.True(t, ok)
	assert.Equal(t, uint64(200), pt)
	// media between the two segments (5120 <= t < 6144) is cut out
	_, ok = video.GetPresentationTime(5632)
	assert.False(t, ok)
	// presentation past the edit list maps nowhere
	_, ok = video.GetMediaTime(700)
	assert.False(t, ok)
}

func TestProbeEditListEmptyAndMultipleSegments(t *testing.T) {
	// video: empty(150) + real(300@2048) + real(350@6144)
	// audio: empty(150) + real(850@1024)
	info := probeFile(t, "sample_empty_multi_edit.mp4")

	video := info.Tracks[0]
	require.Len(t, video.EditList, 3)
	assert.True(t, video.EditList[0].IsEmpty())
	assert.Equal(t, uint64(800), video.GetPresentationDuration())

	// audio timescale differs (44100); verify timescale conversion
	audio := info.Tracks[1]
	require.Len(t, audio.EditList, 2)
	assert.True(t, audio.EditList[0].IsEmpty())
	assert.Equal(t, uint64(1000), audio.GetPresentationDuration())
	// 850 ms of presented audio = 850 * 44100 / 1000 = 37485 media units
	assert.Equal(t, uint64(37485), audio.EditList.GetMediaDuration(info.Timescale, audio.Timescale))

	// first real audio sample is presented after the 150 ms empty edit
	mt, ok := audio.GetMediaTime(150)
	require.True(t, ok)
	assert.Equal(t, uint64(1024), mt)
	// inverse: media 1024 -> presentation 150
	pt, ok := audio.GetPresentationTime(1024)
	require.True(t, ok)
	assert.Equal(t, uint64(150), pt)
}

func TestProbeWithoutEditListUnchanged(t *testing.T) {
	// A track without an edit list keeps its original behavior:
	// Track.Duration still comes from mdhd, and the whole media is presented.
	info := probeFile(t, "sample_no_edit.mp4")
	video := info.Tracks[0]
	require.Empty(t, video.EditList)
	assert.Equal(t, uint32(10240), video.Timescale)
	assert.Equal(t, uint32(1000), video.MovieTimescale)
	// mdhd media duration is untouched
	assert.Equal(t, uint64(10240), video.Duration)
	// presentation duration is the media duration converted to movie units:
	// 10240 * 1000 / 10240 = 1000 (matches tkhd/mvhd duration)
	assert.Equal(t, uint64(1000), video.GetPresentationDuration())

	// without an edit list the conversion is a pure timescale mapping
	mt, ok := video.GetMediaTime(500)
	require.True(t, ok)
	assert.Equal(t, uint64(5120), mt)
	pt, ok := video.GetPresentationTime(5120)
	require.True(t, ok)
	assert.Equal(t, uint64(500), pt)

	// the audio track in the same file still has its edit list, proving the
	// two tracks are handled independently
	assert.NotEmpty(t, info.Tracks[1].EditList)
}

func TestEditListTimeConversionTimescales(t *testing.T) {
	// unit-level checks with differing movie/media timescales
	el := EditList{
		&EditListEntry{MediaTime: -1, SegmentDuration: 500},    // 0.5 s empty
		&EditListEntry{MediaTime: 2000, SegmentDuration: 1000}, // 1 s of media
	}
	const movieTS, mediaTS = uint32(1000), uint32(48000)

	assert.True(t, el[0].IsEmpty())
	assert.False(t, el[1].IsEmpty())
	assert.Equal(t, uint64(1500), el.GetPresentationDuration())
	assert.Equal(t, uint64(48000), el.GetMediaDuration(movieTS, mediaTS))

	// empty interval
	_, ok := el.GetMediaTime(499, movieTS, mediaTS)
	assert.False(t, ok)
	// start of real segment
	mt, ok := el.GetMediaTime(500, movieTS, mediaTS)
	require.True(t, ok)
	assert.Equal(t, uint64(2000), mt)
	// 0.25 s into the real segment -> 12000 media units later
	mt, ok = el.GetMediaTime(750, movieTS, mediaTS)
	require.True(t, ok)
	assert.Equal(t, uint64(14000), mt)
	// inverse
	pt, ok := el.GetPresentationTime(14000, movieTS, mediaTS)
	require.True(t, ok)
	assert.Equal(t, uint64(750), pt)
	// outside edit list
	_, ok = el.GetMediaTime(1500, movieTS, mediaTS)
	assert.False(t, ok)
	// media before the edit is not presented
	_, ok = el.GetPresentationTime(1999, movieTS, mediaTS)
	assert.False(t, ok)

	// zero timescales must not panic
	assert.Equal(t, uint64(0), el.GetMediaDuration(0, mediaTS))
	_, ok = el.GetMediaTime(0, 0, mediaTS)
	assert.False(t, ok)
	_, ok = el.GetPresentationTime(0, movieTS, 0)
	assert.False(t, ok)
}
