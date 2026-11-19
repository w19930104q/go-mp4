package mp4

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
)

// ErrOutOfRange is returned by ExtractRange when the requested time range
// lies outside of the media duration, or when it contains no sample of
// some track.
var ErrOutOfRange = errors.New("time range out of bounds")

// ExtractRange extracts the samples within the half-open time range
// [startTime, endTime) from a non-fragmented MP4 file read from r, and
// writes a new MP4 file containing exactly those samples to w.
//
// startTime and endTime are expressed in units of the movie timescale
// (mvhd.timescale, cf. ProbeInfo.Timescale). A sample is included if and
// only if its decoding time, converted to the movie timescale, lies in
// [startTime, endTime): a sample starting exactly at startTime is included
// and a sample starting exactly at endTime is excluded.
//
// Every track of the source is preserved: track properties (sample
// descriptions, handler, and so on) are copied as-is and the sample data
// bytes are copied without re-encoding, so the samples of each output
// track are identical to the source samples in the range. The timing
// tables (stts, ctts, stss, stsz, stsc, stco/co64, sbgp, sdtp, the edit
// list and the durations of mvhd/tkhd/mdhd) are rebuilt for the extracted
// range, and the decoding timestamps are rebased so that the first
// extracted sample of each track starts at zero.
//
// An error wrapping ErrOutOfRange is returned when endTime exceeds the
// media duration or when the range contains no sample of some track.
// Fragmented MP4 files are not supported.
func ExtractRange(r io.ReadSeeker, w io.WriteSeeker, startTime, endTime uint64) error {
	if startTime >= endTime {
		return fmt.Errorf("invalid time range: start %d is not before end %d", startTime, endTime)
	}

	d := &trimmer{r: r}
	if err := d.scan(r); err != nil {
		return err
	}
	if err := d.selectSamples(startTime, endTime); err != nil {
		return err
	}
	for _, t := range d.tracks {
		t.buildTables(d.movieTimescale)
	}
	d.movieDuration = 0
	for _, t := range d.tracks {
		if t.tkhdDuration > d.movieDuration {
			d.movieDuration = t.tkhdDuration
		}
	}
	return d.write(w)
}

type trimmer struct {
	r              io.ReadSeeker
	topBoxes       []*BoxInfo
	moov           *BoxInfo
	movieTimescale uint64
	mvhdDuration   uint64 // declared media duration in movie timescale
	movieDuration  uint64 // duration of the extracted part in movie timescale
	tracks         []*trimTrack
	trackByOffset  map[uint64]*trimTrack
	cur            *trimTrack // track whose boxes are being written
	chunks         []*trimChunk
	mdatHeaderSize uint64
}

type trimChunk struct {
	srcOffset              uint64
	dstOffset              uint64
	size                   uint64
	samplesPerChunk        uint32
	sampleDescriptionIndex uint32
}

type trimTrack struct {
	id        uint32
	timescale uint32

	tkhd  *Tkhd
	mdhd  *Mdhd
	elst  *Elst // nil if the source track has no edit list
	stts  *Stts
	ctts  *Ctts // nil if none
	stss  *Stss // nil if none
	stsc  *Stsc
	stsz  *Stsz
	stco  *Stco // nil if the source track uses co64
	co64  *Co64 // nil if the source track uses stco
	sdtp  *Sdtp // nil if none
	sbgps []*Sbgp

	// per-sample attributes of the source track
	deltas     []uint32
	sizes      []uint32
	ctss       []int64 // nil if the source track has no ctts
	timestamps []uint64

	startIdx int
	endIdx   int

	chunks       []*trimChunk
	duration     uint64 // duration of the extracted part in track timescale
	tkhdDuration uint64 // duration of the extracted part in movie timescale

	newStts  *Stts
	newCtts  *Ctts
	newStss  *Stss
	newStsz  *Stsz
	newStsc  *Stsc
	newSdtp  *Sdtp
	newSbgps []*Sbgp
	newElst  *Elst

	useCo64    bool
	sbgpIdx    int
	patchPos   uint64 // offset of the first stco/co64 entry in the built moov
	patchLarge bool   // true when the entries are 64 bits (co64)
}

// scan collects the top-level boxes, the movie header and the sample
// tables of every track.
func (d *trimmer) scan(r io.ReadSeeker) error {
	if _, err := ReadBoxStructure(r, func(h *ReadHandle) (interface{}, error) {
		bi := h.BoxInfo
		d.topBoxes = append(d.topBoxes, &bi)
		return nil, nil
	}); err != nil {
		return err
	}

	var mdat *BoxInfo
	for _, bi := range d.topBoxes {
		switch bi.Type {
		case BoxTypeMoov():
			if d.moov == nil {
				d.moov = bi
			}
		case BoxTypeMdat():
			if mdat == nil {
				mdat = bi
			}
		case BoxTypeMoof():
			return errors.New("fragmented MP4 is not supported")
		}
	}
	if d.moov == nil {
		return errors.New("moov box not found")
	}
	if mdat == nil {
		return errors.New("mdat box not found")
	}

	bips, err := ExtractBoxWithPayload(r, d.moov, BoxPath{BoxTypeMvhd()})
	if err != nil {
		return err
	}
	if len(bips) != 1 {
		return errors.New("mvhd box not found")
	}
	mvhd := bips[0].Payload.(*Mvhd)
	if mvhd.Timescale == 0 {
		return errors.New("mvhd timescale is zero")
	}
	d.movieTimescale = uint64(mvhd.Timescale)
	d.mvhdDuration = mvhd.GetDuration()

	trakBIs, err := ExtractBox(r, d.moov, BoxPath{BoxTypeTrak()})
	if err != nil {
		return err
	}
	if len(trakBIs) == 0 {
		return errors.New("no track found")
	}

	d.trackByOffset = make(map[uint64]*trimTrack, len(trakBIs))
	for _, trakBI := range trakBIs {
		track, err := loadTrimTrack(r, trakBI)
		if err != nil {
			return err
		}
		d.tracks = append(d.tracks, track)
		d.trackByOffset[trakBI.Offset] = track
	}
	return nil
}

func loadTrimTrack(r io.ReadSeeker, trakBI *BoxInfo) (*trimTrack, error) {
	stblPath := func(t BoxType) BoxPath {
		return BoxPath{BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), t}
	}
	bips, err := ExtractBoxesWithPayload(r, trakBI, []BoxPath{
		{BoxTypeTkhd()},
		{BoxTypeEdts(), BoxTypeElst()},
		{BoxTypeMdia(), BoxTypeMdhd()},
		stblPath(BoxTypeStts()),
		stblPath(BoxTypeCtts()),
		stblPath(BoxTypeStss()),
		stblPath(BoxTypeStsc()),
		stblPath(BoxTypeStsz()),
		stblPath(BoxTypeStco()),
		stblPath(BoxTypeCo64()),
		stblPath(BoxTypeSdtp()),
		stblPath(BoxTypeSbgp()),
	})
	if err != nil {
		return nil, err
	}

	t := &trimTrack{}
	for _, bip := range bips {
		switch bip.Info.Type {
		case BoxTypeTkhd():
			t.tkhd = bip.Payload.(*Tkhd)
		case BoxTypeElst():
			t.elst = bip.Payload.(*Elst)
		case BoxTypeMdhd():
			t.mdhd = bip.Payload.(*Mdhd)
		case BoxTypeStts():
			t.stts = bip.Payload.(*Stts)
		case BoxTypeCtts():
			t.ctts = bip.Payload.(*Ctts)
		case BoxTypeStss():
			t.stss = bip.Payload.(*Stss)
		case BoxTypeStsc():
			t.stsc = bip.Payload.(*Stsc)
		case BoxTypeStsz():
			t.stsz = bip.Payload.(*Stsz)
		case BoxTypeStco():
			t.stco = bip.Payload.(*Stco)
		case BoxTypeCo64():
			t.co64 = bip.Payload.(*Co64)
		case BoxTypeSdtp():
			t.sdtp = bip.Payload.(*Sdtp)
		case BoxTypeSbgp():
			t.sbgps = append(t.sbgps, bip.Payload.(*Sbgp))
		}
	}

	if t.tkhd == nil {
		return nil, errors.New("tkhd box not found")
	}
	if t.mdhd == nil {
		return nil, errors.New("mdhd box not found")
	}
	if t.stts == nil {
		return nil, errors.New("stts box not found")
	}
	if t.stsc == nil {
		return nil, errors.New("stsc box not found")
	}
	if t.stsz == nil {
		return nil, errors.New("stsz box not found")
	}
	if t.stco == nil && t.co64 == nil {
		return nil, errors.New("stco/co64 box not found")
	}

	t.id = t.tkhd.TrackID
	t.timescale = t.mdhd.Timescale
	if t.timescale == 0 {
		return nil, fmt.Errorf("track %d: mdhd timescale is zero", t.id)
	}

	for _, entry := range t.stts.Entries {
		for i := uint32(0); i < entry.SampleCount; i++ {
			t.deltas = append(t.deltas, entry.SampleDelta)
		}
	}
	n := len(t.deltas)

	t.sizes = make([]uint32, n)
	if t.stsz.SampleSize != 0 {
		for i := range t.sizes {
			t.sizes[i] = t.stsz.SampleSize
		}
	} else {
		if len(t.stsz.EntrySize) < n {
			return nil, fmt.Errorf("track %d: stsz has %d entries for %d samples", t.id, len(t.stsz.EntrySize), n)
		}
		copy(t.sizes, t.stsz.EntrySize)
	}

	if t.ctts != nil {
		t.ctss = make([]int64, n)
		si := 0
		for ci := range t.ctts.Entries {
			for i := uint32(0); i < t.ctts.Entries[ci].SampleCount && si < n; i++ {
				t.ctss[si] = t.ctts.GetSampleOffset(ci)
				si++
			}
		}
	}

	t.timestamps = make([]uint64, n)
	var ts uint64
	for i, delta := range t.deltas {
		t.timestamps[i] = ts
		ts += uint64(delta)
	}
	return t, nil
}

// selectSamples computes the sample index range [startIdx, endIdx) of each
// track whose decoding time lies in [startTime, endTime).
func (d *trimmer) selectSamples(startTime, endTime uint64) error {
	bound := d.mvhdDuration
	for _, t := range d.tracks {
		if dur := ceilDiv(t.mdhd.GetDuration()*d.movieTimescale, uint64(t.timescale)); dur > bound {
			bound = dur
		}
	}
	if endTime > bound {
		return fmt.Errorf("%w: [%d, %d) exceeds the media duration %d", ErrOutOfRange, startTime, endTime, bound)
	}

	for _, t := range d.tracks {
		trkTs := uint64(t.timescale)
		n := len(t.deltas)
		s := sort.Search(n, func(i int) bool {
			return t.timestamps[i]*d.movieTimescale >= startTime*trkTs
		})
		e := sort.Search(n, func(i int) bool {
			return t.timestamps[i]*d.movieTimescale >= endTime*trkTs
		})
		if s >= e {
			return fmt.Errorf("%w: track %d has no sample in [%d, %d)", ErrOutOfRange, t.id, startTime, endTime)
		}
		t.startIdx = s
		t.endIdx = e
	}
	return nil
}

func ceilDiv(a, b uint64) uint64 {
	return (a + b - 1) / b
}

// buildTables rebuilds the sample tables of the track for the selected
// sample range.
func (t *trimTrack) buildTables(movieTimescale uint64) {
	s, e := t.startIdx, t.endIdx
	nSel := e - s

	// stts
	t.newStts = &Stts{FullBox: t.stts.FullBox}
	for _, delta := range t.deltas[s:e] {
		if n := len(t.newStts.Entries); n > 0 && t.newStts.Entries[n-1].SampleDelta == delta {
			t.newStts.Entries[n-1].SampleCount++
		} else {
			t.newStts.Entries = append(t.newStts.Entries, SttsEntry{SampleCount: 1, SampleDelta: delta})
		}
	}
	t.newStts.EntryCount = uint32(len(t.newStts.Entries))

	// ctts: the composition time offsets of the extracted samples are kept
	// as-is so that each sample keeps its original presentation offset.
	if t.ctts != nil {
		t.newCtts = &Ctts{FullBox: t.ctts.FullBox}
		for _, offset := range t.ctss[s:e] {
			if n := len(t.newCtts.Entries); n > 0 && cttsEntryOffset(&t.newCtts.Entries[n-1], t.ctts.GetVersion()) == offset {
				t.newCtts.Entries[n-1].SampleCount++
			} else {
				entry := CttsEntry{SampleCount: 1}
				if t.ctts.GetVersion() == 0 {
					entry.SampleOffsetV0 = uint32(offset)
				} else {
					entry.SampleOffsetV1 = int32(offset)
				}
				t.newCtts.Entries = append(t.newCtts.Entries, entry)
			}
		}
		t.newCtts.EntryCount = uint32(len(t.newCtts.Entries))
	}

	// stss: sync sample numbers are shifted to the extracted range.
	if t.stss != nil {
		t.newStss = &Stss{FullBox: t.stss.FullBox}
		for _, num := range t.stss.SampleNumber {
			idx := int(num) - 1
			if idx >= s && idx < e {
				t.newStss.SampleNumber = append(t.newStss.SampleNumber, uint32(idx-s+1))
			}
		}
		t.newStss.EntryCount = uint32(len(t.newStss.SampleNumber))
	}

	// stsz
	t.newStsz = &Stsz{FullBox: t.stsz.FullBox, SampleCount: uint32(nSel)}
	if t.stsz.SampleSize != 0 {
		t.newStsz.SampleSize = t.stsz.SampleSize
	} else {
		t.newStsz.EntrySize = append([]uint32(nil), t.stsz.EntrySize[s:e]...)
	}

	// sdtp
	if t.sdtp != nil {
		t.newSdtp = &Sdtp{FullBox: t.sdtp.FullBox}
		t.newSdtp.Samples = make([]SdtpSampleElem, nSel)
		if len(t.sdtp.Samples) >= e {
			copy(t.newSdtp.Samples, t.sdtp.Samples[s:e])
		} else if len(t.sdtp.Samples) > s {
			copy(t.newSdtp.Samples, t.sdtp.Samples[s:])
		}
	}

	// sbgp: sample group assignments are expanded per sample, sliced and
	// re-compressed. Samples beyond the declared coverage are unassigned.
	for _, sbgp := range t.sbgps {
		coverage := 0
		for _, entry := range sbgp.Entries {
			coverage += int(entry.SampleCount)
		}
		if coverage > len(t.deltas) {
			coverage = len(t.deltas)
		}
		expanded := make([]uint32, coverage)
		si := 0
		for _, entry := range sbgp.Entries {
			for i := uint32(0); i < entry.SampleCount && si < coverage; i++ {
				expanded[si] = entry.GroupDescriptionIndex
				si++
			}
		}
		nb := &Sbgp{
			FullBox:               sbgp.FullBox,
			GroupingType:          sbgp.GroupingType,
			GroupingTypeParameter: sbgp.GroupingTypeParameter,
		}
		newCoverage := coverage - s
		if newCoverage > nSel {
			newCoverage = nSel
		}
		if newCoverage < 0 {
			newCoverage = 0
		}
		for _, g := range expanded[s : s+newCoverage] {
			if n := len(nb.Entries); n > 0 && nb.Entries[n-1].GroupDescriptionIndex == g {
				nb.Entries[n-1].SampleCount++
			} else {
				nb.Entries = append(nb.Entries, SbgpEntry{SampleCount: 1, GroupDescriptionIndex: g})
			}
		}
		nb.EntryCount = uint32(len(nb.Entries))
		t.newSbgps = append(t.newSbgps, nb)
	}

	// chunks: the original chunk layout is preserved; chunks are narrowed
	// to the extracted samples and empty chunks are dropped.
	var chunkOffsets []uint64
	if t.stco != nil {
		for _, o := range t.stco.ChunkOffset {
			chunkOffsets = append(chunkOffsets, uint64(o))
		}
	} else {
		chunkOffsets = append(chunkOffsets, t.co64.ChunkOffset...)
	}
	numChunks := len(chunkOffsets)
	spc := make([]uint32, numChunks)
	descIdx := make([]uint32, numChunks)
	for si, entry := range t.stsc.Entries {
		end := numChunks
		if si != len(t.stsc.Entries)-1 && int(t.stsc.Entries[si+1].FirstChunk)-1 < end {
			end = int(t.stsc.Entries[si+1].FirstChunk) - 1
		}
		for ci := int(entry.FirstChunk) - 1; ci >= 0 && ci < end; ci++ {
			spc[ci] = entry.SamplesPerChunk
			descIdx[ci] = entry.SampleDescriptionIndex
		}
	}
	sampleIdx := 0
	for ci := 0; ci < numChunks; ci++ {
		cs := sampleIdx
		ce := sampleIdx + int(spc[ci])
		if ce > len(t.deltas) {
			ce = len(t.deltas)
		}
		sampleIdx = ce
		is, ie := cs, ce
		if is < s {
			is = s
		}
		if ie > e {
			ie = e
		}
		if is >= ie {
			continue
		}
		offset := chunkOffsets[ci]
		for i := cs; i < is; i++ {
			offset += uint64(t.sizes[i])
		}
		var size uint64
		for i := is; i < ie; i++ {
			size += uint64(t.sizes[i])
		}
		t.chunks = append(t.chunks, &trimChunk{
			srcOffset:              offset,
			size:                   size,
			samplesPerChunk:        uint32(ie - is),
			sampleDescriptionIndex: descIdx[ci],
		})
	}

	// stsc
	t.newStsc = &Stsc{FullBox: t.stsc.FullBox}
	for i, c := range t.chunks {
		if n := len(t.newStsc.Entries); n > 0 {
			last := &t.newStsc.Entries[n-1]
			if last.SamplesPerChunk == c.samplesPerChunk && last.SampleDescriptionIndex == c.sampleDescriptionIndex {
				continue
			}
		}
		t.newStsc.Entries = append(t.newStsc.Entries, StscEntry{
			FirstChunk:             uint32(i + 1),
			SamplesPerChunk:        c.samplesPerChunk,
			SampleDescriptionIndex: c.sampleDescriptionIndex,
		})
	}
	t.newStsc.EntryCount = uint32(len(t.newStsc.Entries))

	// durations
	for _, delta := range t.deltas[s:e] {
		t.duration += uint64(delta)
	}
	t.tkhdDuration = ceilDiv(t.duration*movieTimescale, uint64(t.timescale))

	// elst: the media time is shifted by the decoding time of the first
	// extracted sample so that the presentation timing of the source is
	// preserved. When the cut point is past the initial edit, the edit
	// list instead starts the presentation at the first composition time
	// of the extracted part.
	if t.elst != nil && len(t.elst.Entries) > 0 {
		ne := &Elst{FullBox: t.elst.FullBox}
		srcMediaTime := t.elst.GetMediaTime(0)
		dts := int64(t.timestamps[s])
		var mediaTime int64
		if srcMediaTime >= dts {
			mediaTime = srcMediaTime - dts
		} else if t.ctss != nil {
			mediaTime = t.ctss[s]
		}
		if ne.GetVersion() == 0 &&
			(t.tkhdDuration > math.MaxUint32 || mediaTime > math.MaxInt32 || mediaTime < math.MinInt32) {
			ne.Version = 1
		}
		entry := ElstEntry{MediaRateInteger: 1}
		if ne.GetVersion() == 0 {
			entry.SegmentDurationV0 = uint32(t.tkhdDuration)
			entry.MediaTimeV0 = int32(mediaTime)
		} else {
			entry.SegmentDurationV1 = t.tkhdDuration
			entry.MediaTimeV1 = mediaTime
		}
		ne.EntryCount = 1
		ne.Entries = []ElstEntry{entry}
		t.newElst = ne
	}
}

func cttsEntryOffset(entry *CttsEntry, version uint8) int64 {
	if version == 0 {
		return int64(entry.SampleOffsetV0)
	}
	return int64(entry.SampleOffsetV1)
}

// write builds the new moov box, computes the output layout and writes the
// extracted file.
func (d *trimmer) write(w io.WriteSeeker) error {
	var moovData []byte
	forceCo64 := false
	for {
		for _, t := range d.tracks {
			t.useCo64 = t.co64 != nil || forceCo64
		}
		data, err := d.buildMoov()
		if err != nil {
			return err
		}
		moovData = data

		d.layout(uint64(len(moovData)))

		overflow := false
		for _, t := range d.tracks {
			if t.useCo64 {
				continue
			}
			for _, c := range t.chunks {
				if c.dstOffset > math.MaxUint32 {
					overflow = true
					break
				}
			}
		}
		if !overflow || forceCo64 {
			break
		}
		forceCo64 = true
	}

	// fill in the chunk offsets of the stco/co64 boxes
	for _, t := range d.tracks {
		for i, c := range t.chunks {
			if t.patchLarge {
				binary.BigEndian.PutUint64(moovData[t.patchPos+uint64(i)*8:], c.dstOffset)
			} else {
				binary.BigEndian.PutUint32(moovData[t.patchPos+uint64(i)*4:], uint32(c.dstOffset))
			}
		}
	}

	// write the output file in the same top-level box order as the source
	writer := NewWriter(w)
	mdatWritten := false
	for _, bi := range d.topBoxes {
		switch bi.Type {
		case BoxTypeMoov():
			if _, err := w.Write(moovData); err != nil {
				return err
			}
		case BoxTypeMdat():
			if mdatWritten {
				continue
			}
			mdatWritten = true
			if err := d.writeMdat(writer); err != nil {
				return err
			}
		case BoxTypeFree(), BoxTypeSkip(), BoxTypeMfra():
			// drop free space and random access information
		default:
			if err := writer.CopyBox(d.r, bi); err != nil {
				return err
			}
		}
	}
	return nil
}

// layout computes the offset of every top-level box of the output and
// assigns the destination offset of every chunk. moovSize is the size of
// the rebuilt moov box.
func (d *trimmer) layout(moovSize uint64) {
	var totalSampleBytes uint64
	for _, t := range d.tracks {
		for _, c := range t.chunks {
			totalSampleBytes += c.size
		}
	}
	mdatHeaderSize := uint64(SmallHeaderSize)
	if totalSampleBytes+mdatHeaderSize > math.MaxUint32 {
		mdatHeaderSize = LargeHeaderSize
	}

	var offset uint64
	var mdatPayloadStart uint64
	mdatWritten := false
	for _, bi := range d.topBoxes {
		var size uint64
		switch bi.Type {
		case BoxTypeMoov():
			size = moovSize
		case BoxTypeMdat():
			if mdatWritten {
				continue
			}
			mdatWritten = true
			mdatPayloadStart = offset + mdatHeaderSize
			size = mdatHeaderSize + totalSampleBytes
		case BoxTypeFree(), BoxTypeSkip(), BoxTypeMfra():
			continue
		default:
			size = bi.Size
		}
		offset += size
	}

	// chunks are written in the order of their source offsets so that the
	// interleaving of the source file is preserved
	var chunks []*trimChunk
	for _, t := range d.tracks {
		chunks = append(chunks, t.chunks...)
	}
	sort.SliceStable(chunks, func(i, j int) bool {
		return chunks[i].srcOffset < chunks[j].srcOffset
	})
	pos := mdatPayloadStart
	for _, c := range chunks {
		c.dstOffset = pos
		pos += c.size
	}
	d.chunks = chunks
	d.mdatHeaderSize = mdatHeaderSize
}

func (d *trimmer) writeMdat(writer *Writer) error {
	var total uint64
	for _, c := range d.chunks {
		total += c.size
	}
	if _, err := writer.StartBox(&BoxInfo{
		Type:       BoxTypeMdat(),
		Size:       d.mdatHeaderSize + total,
		HeaderSize: d.mdatHeaderSize,
	}); err != nil {
		return err
	}
	for _, c := range d.chunks {
		if _, err := d.r.Seek(int64(c.srcOffset), io.SeekStart); err != nil {
			return err
		}
		if _, err := io.CopyN(writer, d.r, int64(c.size)); err != nil {
			return err
		}
	}
	_, err := writer.EndBox()
	return err
}

// buildMoov rebuilds the moov box: every box is copied as-is except the
// timing tables and durations which are replaced by the rebuilt ones.
// The chunk offset entries are left zero and patched later.
func (d *trimmer) buildMoov() ([]byte, error) {
	mws := &memWriteSeeker{}
	writer := NewWriter(mws)
	for _, t := range d.tracks {
		t.sbgpIdx = 0
		t.patchPos = 0
	}
	// the handler is called for the moov box itself and writes it
	if _, err := ReadBoxStructureFromInternal(d.r, d.moov, func(h *ReadHandle) (interface{}, error) {
		return nil, d.handleMoovBox(writer, h)
	}); err != nil {
		return nil, err
	}
	return mws.Bytes(), nil
}

func (d *trimmer) handleMoovBox(writer *Writer, h *ReadHandle) error {
	switch h.BoxInfo.Type {
	case BoxTypeMoov(), BoxTypeTrak(), BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeEdts():
		if h.BoxInfo.Type == BoxTypeTrak() {
			track, ok := d.trackByOffset[h.BoxInfo.Offset]
			if !ok {
				return errors.New("unexpected trak box")
			}
			d.cur = track
		}
		if _, err := writer.StartBox(&BoxInfo{Type: h.BoxInfo.Type}); err != nil {
			return err
		}
		if _, err := h.Expand(); err != nil {
			return err
		}
		if _, err := writer.EndBox(); err != nil {
			return err
		}
		return nil
	}

	t := d.cur
	ctx := h.BoxInfo.Context
	switch h.BoxInfo.Type {
	case BoxTypeMvhd():
		box, _, err := h.ReadPayload()
		if err != nil {
			return err
		}
		mvhd := box.(*Mvhd)
		if mvhd.GetVersion() == 0 {
			mvhd.DurationV0 = uint32(d.movieDuration)
		} else {
			mvhd.DurationV1 = d.movieDuration
		}
		return writeBoxPayload(writer, mvhd, ctx)
	case BoxTypeTkhd():
		box, _, err := h.ReadPayload()
		if err != nil {
			return err
		}
		tkhd := box.(*Tkhd)
		if tkhd.GetVersion() == 0 {
			tkhd.DurationV0 = uint32(t.tkhdDuration)
		} else {
			tkhd.DurationV1 = t.tkhdDuration
		}
		return writeBoxPayload(writer, tkhd, ctx)
	case BoxTypeMdhd():
		box, _, err := h.ReadPayload()
		if err != nil {
			return err
		}
		mdhd := box.(*Mdhd)
		if mdhd.GetVersion() == 0 {
			mdhd.DurationV0 = uint32(t.duration)
		} else {
			mdhd.DurationV1 = t.duration
		}
		return writeBoxPayload(writer, mdhd, ctx)
	case BoxTypeElst():
		if t.newElst == nil {
			return writer.CopyBox(d.r, &h.BoxInfo)
		}
		return writeBoxPayload(writer, t.newElst, ctx)
	case BoxTypeStts():
		return writeBoxPayload(writer, t.newStts, ctx)
	case BoxTypeCtts():
		return writeBoxPayload(writer, t.newCtts, ctx)
	case BoxTypeStss():
		return writeBoxPayload(writer, t.newStss, ctx)
	case BoxTypeStsz():
		return writeBoxPayload(writer, t.newStsz, ctx)
	case BoxTypeStsc():
		return writeBoxPayload(writer, t.newStsc, ctx)
	case BoxTypeSdtp():
		return writeBoxPayload(writer, t.newSdtp, ctx)
	case BoxTypeSbgp():
		if t.sbgpIdx >= len(t.newSbgps) {
			return errors.New("unexpected sbgp box")
		}
		nb := t.newSbgps[t.sbgpIdx]
		t.sbgpIdx++
		return writeBoxPayload(writer, nb, ctx)
	case BoxTypeStco(), BoxTypeCo64():
		return d.writeChunkOffsetBox(writer, t, ctx)
	default:
		return writer.CopyBox(d.r, &h.BoxInfo)
	}
}

func (d *trimmer) writeChunkOffsetBox(writer *Writer, t *trimTrack, ctx Context) error {
	n := len(t.chunks)
	if t.useCo64 {
		fullBox := t.stco.FullBox
		if t.co64 != nil {
			fullBox = t.co64.FullBox
		}
		box := &Co64{
			FullBox:     fullBox,
			EntryCount:  uint32(n),
			ChunkOffset: make([]uint64, n),
		}
		bi, err := writer.StartBox(&BoxInfo{Type: BoxTypeCo64()})
		if err != nil {
			return err
		}
		if _, err := Marshal(writer, box, ctx); err != nil {
			return err
		}
		if _, err := writer.EndBox(); err != nil {
			return err
		}
		t.patchPos = bi.Offset + bi.HeaderSize + 8 // version/flags + entryCount
		t.patchLarge = true
		return nil
	}

	box := &Stco{
		FullBox:     t.stco.FullBox,
		EntryCount:  uint32(n),
		ChunkOffset: make([]uint32, n),
	}
	bi, err := writer.StartBox(&BoxInfo{Type: BoxTypeStco()})
	if err != nil {
		return err
	}
	if _, err := Marshal(writer, box, ctx); err != nil {
		return err
	}
	if _, err := writer.EndBox(); err != nil {
		return err
	}
	t.patchPos = bi.Offset + bi.HeaderSize + 8 // version/flags + entryCount
	t.patchLarge = false
	return nil
}

func writeBoxPayload(writer *Writer, box IBox, ctx Context) error {
	if _, err := writer.StartBox(&BoxInfo{Type: box.GetType()}); err != nil {
		return err
	}
	if _, err := Marshal(writer, box, ctx); err != nil {
		return err
	}
	if _, err := writer.EndBox(); err != nil {
		return err
	}
	return nil
}

// memWriteSeeker is an in-memory io.WriteSeeker.
type memWriteSeeker struct {
	buf  []byte
	pos  int64
	size int64
}

func (m *memWriteSeeker) Write(p []byte) (int, error) {
	end := m.pos + int64(len(p))
	if end > int64(len(m.buf)) {
		newSize := int64(len(m.buf)) * 2
		if newSize < end {
			newSize = end
		}
		newBuf := make([]byte, newSize)
		copy(newBuf, m.buf)
		m.buf = newBuf
	}
	copy(m.buf[m.pos:], p)
	m.pos = end
	if m.pos > m.size {
		m.size = m.pos
	}
	return len(p), nil
}

func (m *memWriteSeeker) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = m.pos + offset
	case io.SeekEnd:
		abs = m.size + offset
	default:
		return 0, fmt.Errorf("invalid whence: %d", whence)
	}
	if abs < 0 {
		return 0, fmt.Errorf("negative position: %d", abs)
	}
	m.pos = abs
	return abs, nil
}

func (m *memWriteSeeker) Bytes() []byte {
	return m.buf[:m.size]
}
