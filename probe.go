package mp4

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/abema/go-mp4/internal/bitio"
)

type ProbeInfo struct {
	MajorBrand       [4]byte
	MinorVersion     uint32
	CompatibleBrands [][4]byte
	FastStart        bool
	Timescale        uint32
	Duration         uint64
	Tracks           Tracks
	Segments         Segments
	SegmentIndexes   SegmentIndexes
}

// SegmentIndexes is a list of segment index information parsed from sidx boxes.
type SegmentIndexes []*SegmentIndex

// SegmentIndex is a summary of one sidx box.
//
// The timescale of an sidx box is independent of the movie/track timescales,
// therefore all time-related fields keep the unit of the originating sidx box
// instead of being converted to a track timebase.
type SegmentIndex struct {
	// Offset is the byte offset of the sidx box itself.
	Offset uint64
	// ReferenceID is the track_ID or another sidx box referenced by this index.
	ReferenceID uint32
	// Timescale is the timescale used by the sidx box.
	Timescale uint32
	// Version is the version field of the sidx box (0 uses 32-bit offsets, 1 uses 64-bit offsets).
	Version uint8
	// EarliestPresentationTime is the earliest presentation time of any media
	// content covered by the sidx box, in the sidx timescale.
	EarliestPresentationTime uint64
	// FirstOffset is the distance, in bytes, from the end of the sidx box to
	// the first referenced box. It is a 64-bit value even for version-0 boxes.
	FirstOffset uint64
	// References summarizes the reference entries of the sidx box.
	References []SegmentIndexReference
}

// SegmentIndexReference is one reference entry of an sidx box.
type SegmentIndexReference struct {
	// ReferencedType is false when the entry references media content (a moof box)
	// and true when it references another sidx box.
	ReferencedType bool
	// ReferencedSize is the byte size of the referenced content.
	ReferencedSize uint32
	// SubsegmentDuration is the duration of the referenced content, in the sidx timescale.
	SubsegmentDuration uint32
	// StartsWithSAP indicates that the referenced content starts with a stream access point.
	StartsWithSAP bool
	// SAPType is the SAP type as defined in ISO/IEC 14496-12.
	SAPType uint32
	// SAPDeltaTime is the SAP delta time, in the sidx timescale.
	SAPDeltaTime uint32
}

// Deprecated: replace with ProbeInfo
type FraProbeInfo = ProbeInfo

type Tracks []*Track

// Deprecated: replace with Track
type TrackInfo = Track

type Track struct {
	TrackID   uint32
	Timescale uint32
	Duration  uint64
	Codec     Codec
	Encrypted bool
	EditList  EditList
	Samples   Samples
	Chunks    Chunks
	AVC       *AVCDecConfigInfo
	MP4A      *MP4AInfo
}

type Codec int

const (
	CodecUnknown Codec = iota
	CodecAVC1
	CodecMP4A
)

type EditList []*EditListEntry

type EditListEntry struct {
	MediaTime       int64
	SegmentDuration uint64
}

type Samples []*Sample

type Sample struct {
	Size                  uint32
	TimeDelta             uint32
	CompositionTimeOffset int64
}

type Chunks []*Chunk

type Chunk struct {
	DataOffset      uint64
	SamplesPerChunk uint32
}

type AVCDecConfigInfo struct {
	ConfigurationVersion uint8
	Profile              uint8
	ProfileCompatibility uint8
	Level                uint8
	LengthSize           uint16
	Width                uint16
	Height               uint16
}

type MP4AInfo struct {
	OTI          uint8
	AudOTI       uint8
	ChannelCount uint16
}

type Segments []*Segment

// Deprecated: replace with Segment
type SegmentInfo = Segment

type Segment struct {
	TrackID               uint32
	MoofOffset            uint64
	BaseMediaDecodeTime   uint64
	DefaultSampleDuration uint32
	SampleCount           uint32
	Duration              uint32
	CompositionTimeOffset int32
	Size                  uint32
}

// Probe probes MP4 file
func Probe(r io.ReadSeeker) (*ProbeInfo, error) {
	probeInfo := &ProbeInfo{
		Tracks:         make([]*Track, 0, 8),
		Segments:       make([]*Segment, 0, 8),
		SegmentIndexes: make([]*SegmentIndex, 0, 2),
	}

	fileSize, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	// Walk the complete box layout, including nested and otherwise unparsed
	// boxes (mfra, free, uuid, ...), so a truncated box is reported as an
	// error instead of being silently ignored. Only box headers are read.
	if err := scanBoxHeaders(r, 0, uint64(fileSize)); err != nil {
		return nil, err
	}

	bis, err := ExtractBoxes(r, nil, []BoxPath{
		{BoxTypeFtyp()},
		{BoxTypeMoov()},
		{BoxTypeMoov(), BoxTypeMvhd()},
		{BoxTypeMoov(), BoxTypeTrak()},
		{BoxTypeMoof()},
		{BoxTypeMdat()},
		{BoxTypeSidx()},
	})
	if err != nil {
		return nil, err
	}
	var mdatAppeared bool
	for _, bi := range bis {
		// A declared box extending past the end of the file means the input is
		// truncated. Return an error instead of reading partial payloads.
		if bi.Offset+bi.Size > uint64(fileSize) {
			return nil, fmt.Errorf("box size exceeds file size: offset=%d, size=%d, fileSize=%d", bi.Offset, bi.Size, fileSize)
		}
		switch bi.Type {
		case BoxTypeFtyp():
			var ftyp Ftyp
			if err := unmarshalBoxPayload(r, bi, &ftyp); err != nil {
				return nil, err
			}
			probeInfo.MajorBrand = ftyp.MajorBrand
			probeInfo.MinorVersion = ftyp.MinorVersion
			probeInfo.CompatibleBrands = make([][4]byte, 0, len(ftyp.CompatibleBrands))
			for _, entry := range ftyp.CompatibleBrands {
				probeInfo.CompatibleBrands = append(probeInfo.CompatibleBrands, entry.CompatibleBrand)
			}
		case BoxTypeMoov():
			probeInfo.FastStart = !mdatAppeared
		case BoxTypeMvhd():
			var mvhd Mvhd
			if err := unmarshalBoxPayload(r, bi, &mvhd); err != nil {
				return nil, err
			}
			probeInfo.Timescale = mvhd.Timescale
			if mvhd.GetVersion() == 0 {
				probeInfo.Duration = uint64(mvhd.DurationV0)
			} else {
				probeInfo.Duration = mvhd.DurationV1
			}
		case BoxTypeTrak():
			track, err := probeTrak(r, bi)
			if err != nil {
				return nil, err
			}
			probeInfo.Tracks = append(probeInfo.Tracks, track)
		case BoxTypeMoof():
			segments, err := probeMoof(r, bi)
			if err != nil {
				return nil, err
			}
			probeInfo.Segments = append(probeInfo.Segments, segments...)
		case BoxTypeSidx():
			index, err := probeSidx(r, bi)
			if err != nil {
				return nil, err
			}
			probeInfo.SegmentIndexes = append(probeInfo.SegmentIndexes, index)
		case BoxTypeMdat():
			mdatAppeared = true
		}
	}
	return probeInfo, nil
}

// unmarshalBoxPayload seeks to the box payload and unmarshals it into dst.
// Boxes whose payload is shorter than their fixed header (for example an
// empty styp box) are skipped safely instead of surfacing an EOF error.
func unmarshalBoxPayload(r io.ReadSeeker, bi *BoxInfo, dst IBox) error {
	if _, err := bi.SeekToPayload(r); err != nil {
		return err
	}
	if err := checkPayloadSize(r, bi); err != nil {
		return err
	}
	payloadSize := bi.Size - bi.HeaderSize
	if payloadSize == 0 {
		return nil
	}
	_, err := Unmarshal(r, payloadSize, dst, bi.Context)
	return err
}

// containerBoxTypes are box types whose payload consists entirely of child
// boxes with no fixed prefix. scanBoxHeaders only recurses into these; every
// other box (including stsd, whose payload starts with an 8-byte sample-entry
// prefix, and sample entries such as avc1/mp4a) is treated as an opaque
// payload and skipped by its declared size.
var containerBoxTypes = map[BoxType]struct{}{
	BoxTypeMoov(): {},
	BoxTypeTrak(): {},
	BoxTypeEdts(): {},
	BoxTypeMdia(): {},
	BoxTypeMinf(): {},
	BoxTypeStbl(): {},
	BoxTypeMvex(): {},
	BoxTypeMoof(): {},
	BoxTypeTraf(): {},
	BoxTypeMfra(): {},
	BoxTypeUdta(): {},
}

// scanBoxHeaders validates that every box declared within [start, start+limit)
// fits entirely inside that range, recursively descending into container
// boxes. It reads only box headers and never unmarshals payloads, so unknown
// or optional boxes are skipped safely. A truncated or over-sized box yields
// an error instead of a panic.
func scanBoxHeaders(r io.ReadSeeker, start, limit uint64) error {
	if _, err := r.Seek(int64(start), io.SeekStart); err != nil {
		return err
	}
	end := start + limit
	pos := start
	sawBox := false
	for pos+SmallHeaderSize <= end {
		bi, err := ReadBoxInfo(r)
		if err != nil {
			// A partial header at the boundary means the file is truncated.
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return fmt.Errorf("truncated box header at offset=%d", pos)
			}
			return err
		}
		sawBox = true
		if bi.Offset+bi.Size > end {
			return fmt.Errorf("box size exceeds containing range: type=%s, offset=%d, size=%d, rangeEnd=%d",
				bi.Type.String(), bi.Offset, bi.Size, end)
		}
		if _, isContainer := containerBoxTypes[bi.Type]; isContainer {
			if err := scanBoxHeaders(r, bi.Offset+bi.HeaderSize, bi.Size-bi.HeaderSize); err != nil {
				return err
			}
		}
		if _, err := r.Seek(int64(bi.Offset+bi.Size), io.SeekStart); err != nil {
			return err
		}
		pos = bi.Offset + bi.Size
	}
	// Any bytes left over inside a container are a truncated child box header
	// rather than valid padding. An empty root range keeps the historical
	// "no boxes" semantics instead of being treated as truncation.
	if sawBox && end-pos != 0 {
		return fmt.Errorf("truncated box header at offset=%d", pos)
	}
	return nil
}

// ProbeFra probes fragmented MP4 file
// Deprecated: replace with Probe
func ProbeFra(r io.ReadSeeker) (*FraProbeInfo, error) {
	probeInfo, err := Probe(r)
	return (*FraProbeInfo)(probeInfo), err
}

func probeTrak(r io.ReadSeeker, bi *BoxInfo) (*Track, error) {
	track := new(Track)

	bips, err := ExtractBoxesWithPayload(r, bi, []BoxPath{
		{BoxTypeTkhd()},
		{BoxTypeEdts(), BoxTypeElst()},
		{BoxTypeMdia(), BoxTypeMdhd()},
		{BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeStsd(), BoxTypeAvc1()},
		{BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeStsd(), BoxTypeAvc1(), BoxTypeAvcC()},
		{BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeStsd(), BoxTypeEncv()},
		{BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeStsd(), BoxTypeEncv(), BoxTypeAvcC()},
		{BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeStsd(), BoxTypeMp4a()},
		{BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeStsd(), BoxTypeMp4a(), BoxTypeEsds()},
		{BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeStsd(), BoxTypeMp4a(), BoxTypeWave(), BoxTypeEsds()},
		{BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeStsd(), BoxTypeEnca()},
		{BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeStsd(), BoxTypeEnca(), BoxTypeEsds()},
		{BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeStco()},
		{BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeCo64()},
		{BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeStts()},
		{BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeCtts()},
		{BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeStsc()},
		{BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeStsz()},
	})
	if err != nil {
		return nil, err
	}
	var tkhd *Tkhd
	var elst *Elst
	var mdhd *Mdhd
	var avc1 *VisualSampleEntry
	var avcC *AVCDecoderConfiguration
	var audioSampleEntry *AudioSampleEntry
	var esds *Esds
	var stco *Stco
	var stts *Stts
	var stsc *Stsc
	var ctts *Ctts
	var stsz *Stsz
	var co64 *Co64
	for _, bip := range bips {
		switch bip.Info.Type {
		case BoxTypeTkhd():
			tkhd = bip.Payload.(*Tkhd)
		case BoxTypeElst():
			elst = bip.Payload.(*Elst)
		case BoxTypeMdhd():
			mdhd = bip.Payload.(*Mdhd)
		case BoxTypeAvc1():
			track.Codec = CodecAVC1
			avc1 = bip.Payload.(*VisualSampleEntry)
		case BoxTypeAvcC():
			avcC = bip.Payload.(*AVCDecoderConfiguration)
		case BoxTypeEncv():
			track.Codec = CodecAVC1
			track.Encrypted = true
		case BoxTypeMp4a():
			track.Codec = CodecMP4A
			audioSampleEntry = bip.Payload.(*AudioSampleEntry)
		case BoxTypeEnca():
			track.Codec = CodecMP4A
			track.Encrypted = true
			audioSampleEntry = bip.Payload.(*AudioSampleEntry)
		case BoxTypeEsds():
			esds = bip.Payload.(*Esds)
		case BoxTypeStco():
			stco = bip.Payload.(*Stco)
		case BoxTypeStts():
			stts = bip.Payload.(*Stts)
		case BoxTypeStsc():
			stsc = bip.Payload.(*Stsc)
		case BoxTypeCtts():
			ctts = bip.Payload.(*Ctts)
		case BoxTypeStsz():
			stsz = bip.Payload.(*Stsz)
		case BoxTypeCo64():
			co64 = bip.Payload.(*Co64)
		}
	}

	if tkhd == nil {
		return nil, errors.New("tkhd box not found")
	}
	track.TrackID = tkhd.TrackID

	if elst != nil {
		editList := make([]*EditListEntry, 0, len(elst.Entries))
		for i := range elst.Entries {
			editList = append(editList, &EditListEntry{
				MediaTime:       elst.GetMediaTime(i),
				SegmentDuration: elst.GetSegmentDuration(i),
			})
		}
		track.EditList = editList
	}

	if mdhd == nil {
		return nil, errors.New("mdhd box not found")
	}
	track.Timescale = mdhd.Timescale
	track.Duration = mdhd.GetDuration()

	if avc1 != nil && avcC != nil {
		track.AVC = &AVCDecConfigInfo{
			ConfigurationVersion: avcC.ConfigurationVersion,
			Profile:              avcC.Profile,
			ProfileCompatibility: avcC.ProfileCompatibility,
			Level:                avcC.Level,
			LengthSize:           uint16(avcC.LengthSizeMinusOne) + 1,
			Width:                avc1.Width,
			Height:               avc1.Height,
		}
	}

	if audioSampleEntry != nil && esds != nil {
		oti, audOTI, err := detectAACProfile(esds)
		if err != nil {
			return nil, err
		}
		track.MP4A = &MP4AInfo{
			OTI:          oti,
			AudOTI:       audOTI,
			ChannelCount: audioSampleEntry.ChannelCount,
		}
	}

	track.Chunks = make([]*Chunk, 0)
	if stco != nil {
		for _, offset := range stco.ChunkOffset {
			track.Chunks = append(track.Chunks, &Chunk{
				DataOffset: uint64(offset),
			})
		}
	} else if co64 != nil {
		for _, offset := range co64.ChunkOffset {
			track.Chunks = append(track.Chunks, &Chunk{
				DataOffset: offset,
			})
		}
	} else {
		return nil, errors.New("stco/co64 box not found")
	}

	if stts == nil {
		return nil, errors.New("stts box not found")
	}
	track.Samples = make([]*Sample, 0)
	for _, entry := range stts.Entries {
		for i := uint32(0); i < entry.SampleCount; i++ {
			track.Samples = append(track.Samples, &Sample{
				TimeDelta: entry.SampleDelta,
			})
		}
	}

	if stsc == nil {
		return nil, errors.New("stsc box not found")
	}
	for si, entry := range stsc.Entries {
		end := uint32(len(track.Chunks))
		if si != len(stsc.Entries)-1 && stsc.Entries[si+1].FirstChunk-1 < end {
			end = stsc.Entries[si+1].FirstChunk - 1
		}
		for ci := entry.FirstChunk - 1; ci < end; ci++ {
			track.Chunks[ci].SamplesPerChunk = entry.SamplesPerChunk
		}
	}

	if ctts != nil {
		var si uint32
		for ci, entry := range ctts.Entries {
			for i := uint32(0); i < entry.SampleCount; i++ {
				if si >= uint32(len(track.Samples)) {
					break
				}
				track.Samples[si].CompositionTimeOffset = ctts.GetSampleOffset(ci)
				si++
			}
		}
	}

	if stsz != nil {
		if stsz.SampleSize != 0 {
			// all samples have the same size and the table is empty
			for i := range track.Samples {
				track.Samples[i].Size = stsz.SampleSize
			}
		} else {
			for i := 0; i < len(stsz.EntrySize) && i < len(track.Samples); i++ {
				track.Samples[i].Size = stsz.EntrySize[i]
			}
		}
	}

	return track, nil
}

func detectAACProfile(esds *Esds) (oti, audOTI uint8, err error) {
	configDscr := findDescriptorByTag(esds.Descriptors, DecoderConfigDescrTag)
	if configDscr == nil || configDscr.DecoderConfigDescriptor == nil {
		return 0, 0, nil
	}
	if configDscr.DecoderConfigDescriptor.ObjectTypeIndication != 0x40 {
		return configDscr.DecoderConfigDescriptor.ObjectTypeIndication, 0, nil
	}

	specificDscr := findDescriptorByTag(esds.Descriptors, DecSpecificInfoTag)
	if specificDscr == nil {
		return 0, 0, errors.New("DecoderSpecificationInfoDescriptor not found")
	}

	r := bitio.NewReader(bytes.NewReader(specificDscr.Data))
	remaining := len(specificDscr.Data) * 8

	// audio object type
	audioObjectType, read, err := getAudioObjectType(r)
	if err != nil {
		return 0, 0, err
	}
	remaining -= read

	// sampling frequency index
	samplingFrequencyIndex, err := r.ReadBits(4)
	if err != nil {
		return 0, 0, err
	}
	remaining -= 4
	if samplingFrequencyIndex[0] == 0x0f {
		if _, err = r.ReadBits(24); err != nil {
			return 0, 0, err
		}
		remaining -= 24
	}

	if audioObjectType == 2 && remaining >= 20 {
		if _, err = r.ReadBits(4); err != nil {
			return 0, 0, err
		}
		remaining -= 4
		syncExtensionType, err := r.ReadBits(11)
		if err != nil {
			return 0, 0, err
		}
		remaining -= 11
		if syncExtensionType[0] == 0x2 && syncExtensionType[1] == 0xb7 {
			extAudioObjectType, _, err := getAudioObjectType(r)
			if err != nil {
				return 0, 0, err
			}
			if extAudioObjectType == 5 || extAudioObjectType == 22 {
				sbr, err := r.ReadBits(1)
				if err != nil {
					return 0, 0, err
				}
				remaining--
				if sbr[0] != 0 {
					if extAudioObjectType == 5 {
						sfi, err := r.ReadBits(4)
						if err != nil {
							return 0, 0, err
						}
						remaining -= 4
						if sfi[0] == 0xf {
							if _, err := r.ReadBits(24); err != nil {
								return 0, 0, err
							}
							remaining -= 24
						}
						if remaining >= 12 {
							syncExtensionType, err := r.ReadBits(11)
							if err != nil {
								return 0, 0, err
							}
							if syncExtensionType[0] == 0x5 && syncExtensionType[1] == 0x48 {
								ps, err := r.ReadBits(1)
								if err != nil {
									return 0, 0, err
								}
								if ps[0] != 0 {
									return 0x40, 29, nil
								}
							}
						}
					}
					return 0x40, 5, nil
				}
			}
		}
	}
	return 0x40, audioObjectType, nil
}

func findDescriptorByTag(dscrs []Descriptor, tag int8) *Descriptor {
	for _, dscr := range dscrs {
		if dscr.Tag == tag {
			return &dscr
		}
	}
	return nil
}

func getAudioObjectType(r bitio.Reader) (byte, int, error) {
	audioObjectType, err := r.ReadBits(5)
	if err != nil {
		return 0, 0, err
	}
	if audioObjectType[0] != 0x1f {
		return audioObjectType[0], 5, nil
	}
	audioObjectType, err = r.ReadBits(6)
	if err != nil {
		return 0, 0, err
	}
	return audioObjectType[0] + 32, 11, nil
}

// probeMoof returns a segment for each track fragment of the moof box.
func probeMoof(r io.ReadSeeker, bi *BoxInfo) ([]*Segment, error) {
	trafs, err := ExtractBox(r, bi, BoxPath{BoxTypeTraf()})
	if err != nil {
		return nil, err
	}

	segments := make([]*Segment, 0, len(trafs))
	for _, traf := range trafs {
		segment, err := probeTraf(r, traf)
		if err != nil {
			return nil, err
		}
		segment.MoofOffset = bi.Offset
		segments = append(segments, segment)
	}
	return segments, nil
}

func probeTraf(r io.ReadSeeker, bi *BoxInfo) (*Segment, error) {
	bips, err := ExtractBoxesWithPayload(r, bi, []BoxPath{
		{BoxTypeTfhd()},
		{BoxTypeTfdt()},
		{BoxTypeTrun()},
	})
	if err != nil {
		return nil, err
	}

	var tfhd *Tfhd
	var tfdt *Tfdt
	var trun *Trun

	segment := &Segment{}
	for _, bip := range bips {
		switch bip.Info.Type {
		case BoxTypeTfhd():
			tfhd = bip.Payload.(*Tfhd)
		case BoxTypeTfdt():
			tfdt = bip.Payload.(*Tfdt)
		case BoxTypeTrun():
			trun = bip.Payload.(*Trun)
		}
	}

	if tfhd == nil {
		return nil, errors.New("tfhd not found")
	}
	segment.TrackID = tfhd.TrackID
	segment.DefaultSampleDuration = tfhd.DefaultSampleDuration

	if tfdt != nil {
		if tfdt.Version == 0 {
			segment.BaseMediaDecodeTime = uint64(tfdt.BaseMediaDecodeTimeV0)
		} else {
			segment.BaseMediaDecodeTime = tfdt.BaseMediaDecodeTimeV1
		}
	}

	if trun == nil {
		// A traf without trun carries no samples; leave the counters at zero.
		return segment, nil
	}
	segment.SampleCount = trun.SampleCount

	// Accumulate every per-sample value in 64 bits and convert once, so a
	// fragment whose totals exceed uint32 results in an explicit error rather
	// than wrapped values. The duration of every sample is resolved through
	// exactly one rule: trun sample-duration when the flag is present,
	// otherwise the tfhd default, keeping fragment duration consistent with
	// the track timebase regardless of how the boxes encode it.
	durationPresent := trun.CheckFlag(0x000100)
	sizePresent := trun.CheckFlag(0x000200)

	var totalDuration uint64
	var totalSize uint64
	var runningDuration uint64
	for ei := range trun.Entries {
		var sampleDuration uint32
		if durationPresent {
			sampleDuration = trun.Entries[ei].SampleDuration
		} else {
			sampleDuration = tfhd.DefaultSampleDuration
		}
		totalDuration += uint64(sampleDuration)

		if sizePresent {
			totalSize += uint64(trun.Entries[ei].SampleSize)
		} else {
			totalSize += uint64(tfhd.DefaultSampleSize)
		}

		offset := int64(runningDuration) + trun.GetSampleCompositionTimeOffset(ei)
		if ei == 0 || offset < int64(segment.CompositionTimeOffset) {
			if offset < math.MinInt32 || offset > math.MaxInt32 {
				return nil, fmt.Errorf("composition time offset overflows int32: %d", offset)
			}
			segment.CompositionTimeOffset = int32(offset)
		}
		runningDuration += uint64(sampleDuration)
	}

	// Samples without trun entries (SampleCount larger than the entry list)
	// fall back to the tfhd defaults.
	missing := uint64(0)
	if trun.SampleCount > uint32(len(trun.Entries)) {
		missing = uint64(trun.SampleCount) - uint64(len(trun.Entries))
	}
	totalDuration += missing * uint64(tfhd.DefaultSampleDuration)
	totalSize += missing * uint64(tfhd.DefaultSampleSize)

	if totalDuration > math.MaxUint32 {
		return nil, fmt.Errorf("segment duration overflows uint32: %d", totalDuration)
	}
	if totalSize > math.MaxUint32 {
		return nil, fmt.Errorf("segment size overflows uint32: %d", totalSize)
	}
	segment.Duration = uint32(totalDuration)
	segment.Size = uint32(totalSize)

	return segment, nil
}

// probeSidx summarizes one sidx box. Both version 0 (32-bit) and version 1
// (64-bit) boxes are normalized into the same SegmentIndex shape.
func probeSidx(r io.ReadSeeker, bi *BoxInfo) (*SegmentIndex, error) {
	var sidx Sidx
	if err := unmarshalBoxPayload(r, bi, &sidx); err != nil {
		return nil, err
	}

	index := &SegmentIndex{
		Offset:                   bi.Offset,
		ReferenceID:              sidx.ReferenceID,
		Timescale:                sidx.Timescale,
		Version:                  sidx.GetVersion(),
		EarliestPresentationTime: sidx.GetEarliestPresentationTime(),
		FirstOffset:              sidx.GetFirstOffset(),
		References:               make([]SegmentIndexReference, 0, len(sidx.References)),
	}
	for _, ref := range sidx.References {
		index.References = append(index.References, SegmentIndexReference{
			ReferencedType:     ref.ReferenceType,
			ReferencedSize:     ref.ReferencedSize,
			SubsegmentDuration: ref.SubsegmentDuration,
			StartsWithSAP:      ref.StartsWithSAP,
			SAPType:            ref.SAPType,
			SAPDeltaTime:       ref.SAPDeltaTime,
		})
	}
	return index, nil
}

func FindIDRFrames(r io.ReadSeeker, trackInfo *TrackInfo) ([]int, error) {
	if trackInfo.AVC == nil {
		return nil, nil
	}
	lengthSize := uint32(trackInfo.AVC.LengthSize)

	var si int
	idxs := make([]int, 0, 8)
	for _, chunk := range trackInfo.Chunks {
		end := si + int(chunk.SamplesPerChunk)
		dataOffset := chunk.DataOffset
		for ; si < end && si < len(trackInfo.Samples); si++ {
			sample := trackInfo.Samples[si]
			if sample.Size == 0 {
				continue
			}
			for nalOffset := uint32(0); nalOffset+lengthSize+1 <= sample.Size; {
				if _, err := r.Seek(int64(dataOffset+uint64(nalOffset)), io.SeekStart); err != nil {
					return nil, err
				}
				data := make([]byte, lengthSize+1)
				if _, err := io.ReadFull(r, data); err != nil {
					return nil, err
				}
				var length uint32
				for i := 0; i < int(lengthSize); i++ {
					length = (length << 8) + uint32(data[i])
				}
				nalHeader := data[lengthSize]
				nalType := nalHeader & 0x1f
				if nalType == 5 {
					idxs = append(idxs, si)
					break
				}
				nalOffset += lengthSize + length
			}
			dataOffset += uint64(sample.Size)
		}
	}
	return idxs, nil
}

func (samples Samples) GetBitrate(timescale uint32) uint64 {
	var totalSize uint64
	var totalDuration uint64
	for _, sample := range samples {
		totalSize += uint64(sample.Size)
		totalDuration += uint64(sample.TimeDelta)
	}
	if totalDuration == 0 {
		return 0
	}
	return 8 * totalSize * uint64(timescale) / totalDuration
}

func (samples Samples) GetMaxBitrate(timescale uint32, timeDelta uint64) uint64 {
	if timeDelta == 0 {
		return 0
	}
	var maxBitrate uint64
	var size uint64
	var duration uint64
	var begin int
	var end int
	for end < len(samples) {
		for {
			size += uint64(samples[end].Size)
			duration += uint64(samples[end].TimeDelta)
			end++
			if duration >= timeDelta || end == len(samples) {
				break
			}
		}
		if duration != 0 {
			bitrate := 8 * size * uint64(timescale) / duration
			if bitrate > maxBitrate {
				maxBitrate = bitrate
			}
		}
		for {
			size -= uint64(samples[begin].Size)
			duration -= uint64(samples[begin].TimeDelta)
			begin++
			if duration < timeDelta {
				break
			}
		}
	}
	return maxBitrate
}

func (segments Segments) GetBitrate(trackID uint32, timescale uint32) uint64 {
	var totalSize uint64
	var totalDuration uint64
	for _, segment := range segments {
		if segment.TrackID == trackID {
			totalSize += uint64(segment.Size)
			totalDuration += uint64(segment.Duration)
		}
	}
	if totalDuration == 0 {
		return 0
	}
	return 8 * totalSize * uint64(timescale) / totalDuration
}

func (segments Segments) GetMaxBitrate(trackID uint32, timescale uint32) uint64 {
	var maxBitrate uint64
	for _, segment := range segments {
		if segment.TrackID == trackID && segment.Duration != 0 {
			bitrate := 8 * uint64(segment.Size) * uint64(timescale) / uint64(segment.Duration)
			if bitrate > maxBitrate {
				maxBitrate = bitrate
			}
		}
	}
	return maxBitrate
}
