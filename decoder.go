package mp4

import (
	"errors"
	"fmt"
	"io"
	"sync"
)

// BoxDecoderFunc decodes the payload of one box.
//
// r is positioned at the beginning of the box payload (immediately after the
// box header) and payloadSize is the payload size in bytes. The decoder reads
// the fields it needs from r and returns the decoded box together with the
// number of bytes it consumed. When the box also contains child boxes, the
// decoder must consume only its own fields; the bytes left after the returned
// count are treated as child boxes by ReadBoxStructure.
//
// The returned box must implement IBox. Implementations usually embed Box or
// FullBox and return the registered BoxType from GetType.
type BoxDecoderFunc func(r io.ReadSeeker, boxType BoxType, payloadSize uint64, ctx Context) (box IBox, n uint64, err error)

var (
	errBoxDecoderAlreadyRegistered = errors.New("box decoder already registered")

	boxDecoderMu sync.RWMutex
	// boxDecoderMap stores decoders registered at runtime via RegisterBoxDecoder.
	// It is kept separate from boxMap so that built-in definitions and their
	// init-time registration remain untouched.
	boxDecoderMap = make(map[BoxType]BoxDecoderFunc)
)

// RegisterBoxDecoder registers a decoder function for the given box type.
// The registration is safe for concurrent use with parsing and with other
// registrations. It returns an error when a decoder is already registered for
// the box type; use UnregisterBoxDecoder first to replace one.
//
// A registered decoder takes precedence over the built-in struct-based
// definitions, while box types without a registered decoder keep the default
// behavior (built-in parsing or transparent pass-through for unknown types).
func RegisterBoxDecoder(boxType BoxType, decoder BoxDecoderFunc) error {
	if decoder == nil {
		return errors.New("decoder must not be nil")
	}

	boxDecoderMu.Lock()
	defer boxDecoderMu.Unlock()

	if _, exists := boxDecoderMap[boxType]; exists {
		return fmt.Errorf("%w: type=%s", errBoxDecoderAlreadyRegistered, boxType.String())
	}
	boxDecoderMap[boxType] = decoder
	return nil
}

// MustRegisterBoxDecoder registers a decoder function and panics on error.
// It is intended for use in init functions or package-level setup.
func MustRegisterBoxDecoder(boxType BoxType, decoder BoxDecoderFunc) {
	if err := RegisterBoxDecoder(boxType, decoder); err != nil {
		panic(err)
	}
}

// UnregisterBoxDecoder removes the decoder registered for the given box type.
// It returns true when a decoder was removed and false when none was registered.
// The operation is safe for concurrent use with parsing and registration.
func UnregisterBoxDecoder(boxType BoxType) bool {
	boxDecoderMu.Lock()
	defer boxDecoderMu.Unlock()

	if _, exists := boxDecoderMap[boxType]; !exists {
		return false
	}
	delete(boxDecoderMap, boxType)
	return true
}

// IsBoxDecoderRegistered reports whether a runtime decoder is registered for
// the given box type. Built-in box definitions are not considered.
func IsBoxDecoderRegistered(boxType BoxType) bool {
	boxDecoderMu.RLock()
	defer boxDecoderMu.RUnlock()

	_, exists := boxDecoderMap[boxType]
	return exists
}

// lookupBoxDecoder returns the registered decoder, or nil when the box type
// has no runtime decoder.
func lookupBoxDecoder(boxType BoxType) BoxDecoderFunc {
	boxDecoderMu.RLock()
	defer boxDecoderMu.RUnlock()

	return boxDecoderMap[boxType]
}

// unmarshalWithRegisteredDecoder decodes a box through its runtime-registered
// decoder. It returns ok=false when no decoder is registered for the box type,
// in which case the caller falls back to the built-in behavior.
func unmarshalWithRegisteredDecoder(r io.ReadSeeker, boxType BoxType, payloadSize uint64, ctx Context) (box IBox, n uint64, ok bool, err error) {
	decoder := lookupBoxDecoder(boxType)
	if decoder == nil {
		return nil, 0, false, nil
	}

	box, n, err = decoder(r, boxType, payloadSize, ctx)
	if err != nil {
		return nil, 0, true, err
	}
	if box == nil {
		return nil, 0, true, errors.New("registered box decoder returned nil box")
	}
	if n > payloadSize {
		return nil, 0, true, fmt.Errorf("registered box decoder overrun: type=%s, consumed=%d, payloadSize=%d", boxType.String(), n, payloadSize)
	}
	return box, n, true, nil
}
