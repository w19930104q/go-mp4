package mp4

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// customBox is a fictional business-defined box type ("cust").
// Payload layout:
//
//	version  uint8
//	flags    [3]byte
//	value    uint16
//	raw      remaining bytes
type customBox struct {
	FullBox
	Value uint16
	Raw   []byte
}

func boxTypeCust() BoxType { return StrToBoxType("cust") }

func (*customBox) GetType() BoxType { return boxTypeCust() }

func decodeCustomBox(r io.ReadSeeker, boxType BoxType, payloadSize uint64, ctx Context) (IBox, uint64, error) {
	header := make([]byte, 6) // version(1) + flags(3) + value(2)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, 0, err
	}

	box := &customBox{
		Value: binary.BigEndian.Uint16(header[4:6]),
	}
	box.Version = header[0]
	box.Flags = [3]byte{header[1], header[2], header[3]}

	rawSize := int(payloadSize) - len(header)
	if rawSize < 0 {
		return nil, 0, errors.New("cust box payload too short")
	}
	box.Raw = make([]byte, rawSize)
	if _, err := io.ReadFull(r, box.Raw); err != nil {
		return nil, 0, err
	}

	return box, payloadSize, nil
}

// buildBox wraps payload with an 8-byte box header.
func buildBox(boxType BoxType, payload []byte) []byte {
	buf := bytes.NewBuffer(nil)
	header := make([]byte, SmallHeaderSize)
	binary.BigEndian.PutUint32(header, uint32(SmallHeaderSize+len(payload)))
	copy(header[4:], boxType[:])
	buf.Write(header)
	buf.Write(payload)
	return buf.Bytes()
}

func buildCustomBoxSample() []byte {
	payload := []byte{
		0x01,             // version
		0x00, 0x00, 0x07, // flags
		0x12, 0x34, // value
		'a', 'b', 'c', 'd', 'e', // raw
	}
	return buildBox(boxTypeCust(), payload)
}

func TestRegisterBoxDecoderLifecycle(t *testing.T) {
	bt := StrToBoxType("lc01")

	// unregistered: not supported and not found
	require.False(t, IsBoxDecoderRegistered(bt))
	require.False(t, bt.IsSupported(Context{}))
	require.False(t, UnregisterBoxDecoder(bt))

	require.NoError(t, RegisterBoxDecoder(bt, decodeCustomBox))
	// duplicate registration is rejected
	err := RegisterBoxDecoder(bt, decodeCustomBox)
	require.Error(t, err)
	assert.True(t, errors.Is(err, errBoxDecoderAlreadyRegistered))
	// nil decoder is rejected
	require.Error(t, RegisterBoxDecoder(StrToBoxType("lc02"), nil))

	require.True(t, IsBoxDecoderRegistered(bt))
	require.True(t, bt.IsSupported(Context{}))

	require.True(t, UnregisterBoxDecoder(bt))
	require.False(t, IsBoxDecoderRegistered(bt))
	require.False(t, bt.IsSupported(Context{}))
}

func TestMustRegisterBoxDecoder(t *testing.T) {
	bt := StrToBoxType("lc03")
	require.NotPanics(t, func() { MustRegisterBoxDecoder(bt, decodeCustomBox) })
	require.True(t, IsBoxDecoderRegistered(bt))
	require.Panics(t, func() { MustRegisterBoxDecoder(bt, decodeCustomBox) })
	UnregisterBoxDecoder(bt)
}

func TestRegisteredDecoderUnmarshalAny(t *testing.T) {
	require.NoError(t, RegisterBoxDecoder(boxTypeCust(), decodeCustomBox))
	defer UnregisterBoxDecoder(boxTypeCust())

	sample := buildCustomBoxSample()
	r := bytes.NewReader(sample)

	bi, err := ReadBoxInfo(r)
	require.NoError(t, err)
	require.Equal(t, boxTypeCust(), bi.Type)
	require.True(t, bi.IsSupportedType())

	payloadSize := bi.Size - bi.HeaderSize
	box, n, err := UnmarshalAny(r, bi.Type, payloadSize, bi.Context)
	require.NoError(t, err)
	assert.Equal(t, payloadSize, n)

	cust, ok := box.(*customBox)
	require.True(t, ok)
	assert.Equal(t, uint8(1), cust.GetVersion())
	assert.Equal(t, uint32(7), cust.GetFlags())
	assert.Equal(t, uint16(0x1234), cust.Value)
	assert.Equal(t, []byte("abcde"), cust.Raw)
	// repeated parsing of the same sample yields stable results
	box2, _, err := UnmarshalAny(bytes.NewReader(sample[SmallHeaderSize:]), bi.Type, payloadSize, bi.Context)
	require.NoError(t, err)
	assert.Equal(t, box, box2)
}

func TestRegisteredDecoderReadBoxStructure(t *testing.T) {
	require.NoError(t, RegisterBoxDecoder(boxTypeCust(), decodeCustomBox))
	defer UnregisterBoxDecoder(boxTypeCust())

	sample := buildCustomBoxSample()

	var visited int
	vals, err := ReadBoxStructure(bytes.NewReader(sample), func(h *ReadHandle) (interface{}, error) {
		visited++
		require.True(t, h.BoxInfo.IsSupportedType())
		require.Equal(t, boxTypeCust(), h.BoxInfo.Type)

		// the parsed result is stably accessible through ReadPayload
		box, n, err := h.ReadPayload()
		require.NoError(t, err)
		require.Equal(t, h.BoxInfo.Size-h.BoxInfo.HeaderSize, n)

		cust := box.(*customBox)
		assert.Equal(t, uint16(0x1234), cust.Value)
		assert.Equal(t, []byte("abcde"), cust.Raw)

		// ReadData still works and returns the untouched payload
		buf := bytes.NewBuffer(nil)
		dn, err := h.ReadData(buf)
		require.NoError(t, err)
		assert.Equal(t, h.BoxInfo.Size-h.BoxInfo.HeaderSize, dn)
		assert.Equal(t, sample[SmallHeaderSize:], buf.Bytes())
		return cust, nil
	})
	require.NoError(t, err)
	require.Len(t, vals, 1)
	assert.Equal(t, 1, visited)
	assert.IsType(t, &customBox{}, vals[0])
	assert.Equal(t, uint16(0x1234), vals[0].(*customBox).Value)
}

func TestRegisteredDecoderWithChildBoxes(t *testing.T) {
	// The decoder consumes only its own 6-byte header; the trailing bytes
	// form child boxes and must be reachable through Expand.
	child := buildBox(BoxTypeFree(), []byte{0xaa, 0xbb})
	payload := append([]byte{0x00, 0x00, 0x00, 0x00, 0x12, 0x34}, child...)
	sample := buildBox(boxTypeCust(), payload)

	require.NoError(t, RegisterBoxDecoder(boxTypeCust(), func(r io.ReadSeeker, boxType BoxType, payloadSize uint64, ctx Context) (IBox, uint64, error) {
		header := make([]byte, 6)
		if _, err := io.ReadFull(r, header); err != nil {
			return nil, 0, err
		}
		return &customBox{Value: binary.BigEndian.Uint16(header[4:6])}, 6, nil
	}))
	defer UnregisterBoxDecoder(boxTypeCust())

	var sawChild bool
	_, err := ReadBoxStructure(bytes.NewReader(sample), func(h *ReadHandle) (interface{}, error) {
		if h.BoxInfo.Type == BoxTypeFree() {
			sawChild = true
			return nil, nil
		}
		box, _, err := h.ReadPayload()
		require.NoError(t, err)
		assert.Equal(t, uint16(0x1234), box.(*customBox).Value)
		children, err := h.Expand()
		require.NoError(t, err)
		assert.Len(t, children, 1)
		return nil, nil
	})
	require.NoError(t, err)
	assert.True(t, sawChild)
}

func TestUnregisteredTypeRemainsTransparent(t *testing.T) {
	unknown := StrToBoxType("zzzz")
	require.False(t, IsBoxDecoderRegistered(unknown))

	payload := []byte{1, 2, 3, 4, 5}
	sample := buildBox(unknown, payload)

	var visited int
	_, err := ReadBoxStructure(bytes.NewReader(sample), func(h *ReadHandle) (interface{}, error) {
		visited++
		require.False(t, h.BoxInfo.IsSupportedType())

		// payload reading of an unregistered type keeps failing
		_, _, err := h.ReadPayload()
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrBoxInfoNotFound))

		// raw pass-through stays available
		buf := bytes.NewBuffer(nil)
		n, err := h.ReadData(buf)
		require.NoError(t, err)
		assert.Equal(t, uint64(len(payload)), n)
		assert.Equal(t, payload, buf.Bytes())
		return nil, nil
	})
	require.NoError(t, err)
	assert.Equal(t, 1, visited)

	// and registering a decoder flips the behavior without touching other types
	require.NoError(t, RegisterBoxDecoder(unknown, func(r io.ReadSeeker, boxType BoxType, payloadSize uint64, ctx Context) (IBox, uint64, error) {
		box := &customBox{}
		box.Raw = make([]byte, payloadSize)
		if _, err := io.ReadFull(r, box.Raw); err != nil {
			return nil, 0, err
		}
		return box, payloadSize, nil
	}))
	defer UnregisterBoxDecoder(unknown)
	assert.True(t, unknown.IsSupported(Context{}))
}

func TestBuiltinBoxParsingUnchanged(t *testing.T) {
	// "free" is a built-in box and must keep its built-in parsing.
	sample := buildBox(BoxTypeFree(), []byte{0x11, 0x22, 0x33})

	_, err := ReadBoxStructure(bytes.NewReader(sample), func(h *ReadHandle) (interface{}, error) {
		require.True(t, h.BoxInfo.IsSupportedType())
		box, _, err := h.ReadPayload()
		require.NoError(t, err)
		_, isFree := box.(*Free)
		assert.True(t, isFree)
		return nil, nil
	})
	require.NoError(t, err)

	// registering a decoder for a built-in type overrides only that type
	require.NoError(t, RegisterBoxDecoder(BoxTypeFree(), func(r io.ReadSeeker, boxType BoxType, payloadSize uint64, ctx Context) (IBox, uint64, error) {
		box := &customBox{Raw: make([]byte, payloadSize)}
		if _, err := io.ReadFull(r, box.Raw); err != nil {
			return nil, 0, err
		}
		return box, payloadSize, nil
	}))
	_, err = ReadBoxStructure(bytes.NewReader(sample), func(h *ReadHandle) (interface{}, error) {
		box, _, err := h.ReadPayload()
		require.NoError(t, err)
		cust, ok := box.(*customBox)
		require.True(t, ok)
		assert.Equal(t, []byte{0x11, 0x22, 0x33}, cust.Raw)
		return nil, nil
	})
	require.NoError(t, err)

	// removing the registration restores the built-in parser
	require.True(t, UnregisterBoxDecoder(BoxTypeFree()))
	_, err = ReadBoxStructure(bytes.NewReader(sample), func(h *ReadHandle) (interface{}, error) {
		box, _, err := h.ReadPayload()
		require.NoError(t, err)
		_, isFree := box.(*Free)
		assert.True(t, isFree)
		return nil, nil
	})
	require.NoError(t, err)
}

func TestRegisteredDecoderExtractWithPayload(t *testing.T) {
	require.NoError(t, RegisterBoxDecoder(boxTypeCust(), decodeCustomBox))
	defer UnregisterBoxDecoder(boxTypeCust())

	sample := buildCustomBoxSample()

	results, err := ExtractBoxesWithPayload(bytes.NewReader(sample), nil, []BoxPath{{boxTypeCust()}})
	require.NoError(t, err)
	require.Len(t, results, 1)

	assert.Equal(t, boxTypeCust(), results[0].Info.Type)
	cust, ok := results[0].Payload.(*customBox)
	require.True(t, ok)
	assert.Equal(t, uint8(1), cust.GetVersion())
	assert.Equal(t, uint32(7), cust.GetFlags())
	assert.Equal(t, uint16(0x1234), cust.Value)
	assert.Equal(t, []byte("abcde"), cust.Raw)
}

func TestRegisteredDecoderErrors(t *testing.T) {
	bt := StrToBoxType("lc04")

	// decoder failure must propagate
	require.NoError(t, RegisterBoxDecoder(bt, func(r io.ReadSeeker, boxType BoxType, payloadSize uint64, ctx Context) (IBox, uint64, error) {
		return nil, 0, errors.New("boom")
	}))
	sample := buildBox(bt, []byte{1, 2})
	_, err := ReadBoxStructure(bytes.NewReader(sample), func(h *ReadHandle) (interface{}, error) {
		_, _, perr := h.ReadPayload()
		require.EqualError(t, perr, "boom")
		return nil, perr
	})
	require.EqualError(t, err, "boom")
	UnregisterBoxDecoder(bt)

	// decoder claiming more bytes than the payload is rejected
	require.NoError(t, RegisterBoxDecoder(bt, func(r io.ReadSeeker, boxType BoxType, payloadSize uint64, ctx Context) (IBox, uint64, error) {
		return &customBox{}, payloadSize + 1, nil
	}))
	_, _, err = UnmarshalAny(bytes.NewReader(sample), bt, 2, Context{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "overrun")
	UnregisterBoxDecoder(bt)

	// nil box is rejected
	require.NoError(t, RegisterBoxDecoder(bt, func(r io.ReadSeeker, boxType BoxType, payloadSize uint64, ctx Context) (IBox, uint64, error) {
		return nil, 1, nil
	}))
	_, _, err = UnmarshalAny(bytes.NewReader(sample), bt, 2, Context{})
	require.Error(t, err)
	UnregisterBoxDecoder(bt)
}

func TestRegisteredDecoderConcurrent(t *testing.T) {
	const typesN = 8
	types := make([]BoxType, typesN)
	samples := make([][]byte, typesN)
	for i := range types {
		types[i] = Uint32ToBoxType(0x63630000 + uint32(i)) // "cc\0x"
		// payload: version(1) + flags(3) + value(2) = type[3] high byte, 0 + raw
		samples[i] = buildBox(types[i], []byte{0x00, 0x00, 0x00, 0x00, 0x00, byte(i), 'x'})
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 4)

	// registrators keep registering and unregistering while parsing runs.
	for i := range types {
		bt := types[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if err := RegisterBoxDecoder(bt, decodeCustomBox); err != nil {
					errCh <- err
					return
				}
				if !IsBoxDecoderRegistered(bt) {
					errCh <- errors.New("decoder missing after registration")
					return
				}
				UnregisterBoxDecoder(bt)
			}
		}()
	}

	// readers parse the samples concurrently and assert stable, deterministic
	// results whenever a decoder happens to be registered.
	for i := range types {
		sample := samples[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				r := bytes.NewReader(sample)
				bi, err := ReadBoxInfo(r)
				if err != nil {
					errCh <- err
					return
				}
				box, n, err := UnmarshalAny(r, bi.Type, bi.Size-bi.HeaderSize, bi.Context)
				if errors.Is(err, ErrBoxInfoNotFound) {
					continue // decoder currently unregistered: transparent fallback
				}
				if err != nil {
					errCh <- err
					return
				}
				cust, ok := box.(*customBox)
				if !ok {
					errCh <- errors.New("unexpected box type")
					return
				}
				if n != bi.Size-bi.HeaderSize || cust.Value != uint16(bi.Type[3]) {
					errCh <- errors.New("unstable parse result")
					return
				}
			}
		}()
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		require.NoError(t, err)
	}
}
