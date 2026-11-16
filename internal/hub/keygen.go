package hub

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
)

// generatedKeyLength is the size of one security key before encryption: 16
// bytes, 32 hex characters (data-model.md §3).
const generatedKeyLength = 16

// keyDrawAttempts bounds how often one class redraws after a value it rejected.
// crypto/rand does not produce an all-zero or all-0xFF draw, so the cap only
// keeps a pathological reader from spinning forever; it is generous enough that
// a real draw never reaches it (R15).
const keyDrawAttempts = 64

// GenerateClasses resolves a generation request against the classes the
// driver's option object carries (data-model.md §4). An empty or nil request
// means the four classic classes in canonical order, because first activation
// provisions those alone (FR-008, R15). A non-empty request keeps only the
// classes it names, each of them once, in canonical order — the classic classes
// first, then the Long Range pair — so neither the request order nor its
// repeats reach the driver (FR-010). An unknown name refuses the whole request
// with no classes at all, and Long Range is never added implicitly: it exists
// only once an operator asks for it (FR-008).
func GenerateClasses(requested []string) ([]string, error) {
	if len(requested) == 0 {
		return ClassicClasses(), nil
	}

	canonical := AllClasses()
	wanted := make(map[string]struct{}, len(requested))
	for _, class := range requested {
		if !knownClass(class) {
			return nil, fmt.Errorf("hub: generate classes: %q is not a security class", class)
		}
		wanted[class] = struct{}{}
	}

	// Walking the canonical order rather than the request is what puts the
	// classic classes first and drops the repeats.
	resolved := make([]string, 0, len(wanted))
	for _, class := range canonical {
		if _, ok := wanted[class]; ok {
			resolved = append(resolved, class)
		}
	}
	return resolved, nil
}

// GenerateKeys draws fresh material from crypto/rand for the classes it is
// given. classes is the resolution GenerateClasses returned, so an empty or nil
// list is a caller mistake rather than a request for the defaults, and a name
// outside the known set is refused. A failure returns no map at all: a set with
// a class missing is unusable, so a partial one must never reach the store
// (R15).
func GenerateKeys(classes []string) (map[string][]byte, error) {
	for _, class := range classes {
		if !knownClass(class) {
			return nil, fmt.Errorf("hub: generate keys: %q is not a security class", class)
		}
	}
	return generateFrom(rand.Reader, classes)
}

// generateFrom is the seam the tests drive. It draws 16 bytes per class from r
// and discards a draw that is all zero or all 0xFF, taking the redraw from the
// same reader so a test can hand in a rejected value followed by a good one.
// The assertion costs a line and crypto/rand cannot fail it, but a key of
// either shape is the one value R15 refuses to hand back. An empty class
// list, a reader that runs out, and a run of rejected draws longer than
// keyDrawAttempts are all errors with no keys, because an incomplete set must
// never look usable.
func generateFrom(r io.Reader, classes []string) (map[string][]byte, error) {
	if len(classes) == 0 {
		return nil, errors.New("hub: generate keys: no security classes given")
	}

	keys := make(map[string][]byte, len(classes))
	for _, class := range classes {
		value, err := drawKey(r, class)
		if err != nil {
			return nil, err
		}
		keys[class] = value
	}
	return keys, nil
}

// drawKey draws one accepted value for class, redrawing while r hands back a
// value that is all zero or all 0xFF. An error names the class and never the
// material: a key value has no place in a message the API may surface (FR-011,
// FR-013).
func drawKey(r io.Reader, class string) ([]byte, error) {
	for attempt := 0; attempt < keyDrawAttempts; attempt++ {
		value := make([]byte, generatedKeyLength)
		if _, err := io.ReadFull(r, value); err != nil {
			return nil, fmt.Errorf("hub: draw a key for %s: %w", class, err)
		}
		if !rejectedKeyDraw(value) {
			return value, nil
		}
	}
	return nil, fmt.Errorf("hub: draw a key for %s: %d draws were all zero or all 0xFF", class, keyDrawAttempts)
}

// rejectedKeyDraw reports whether value is one of the two shapes R15 discards.
// It compares the whole value rather than its first byte alone, because a key
// that merely begins with 0x00 is a good key and must be kept.
func rejectedKeyDraw(value []byte) bool {
	if len(value) == 0 {
		return false
	}
	first := value[0]
	if first != 0x00 && first != 0xFF {
		return false
	}
	for _, b := range value[1:] {
		if b != first {
			return false
		}
	}
	return true
}

// Activation is what the first-activation trigger knows when a coordinator
// reports ready: how it is attached, whether a key is already on record, and
// what network the controller is carrying.
type Activation struct {
	// Connection is the state the driver reported for the coordinator.
	Connection Connection
	// KeyRevision is the committed key revision; 0 means no key row exists.
	KeyRevision int
	// Configured is how many classes already hold a key.
	Configured int
	// NodeCount is the node count the coordinator reported at start-up.
	NodeCount int
}

// ShouldGenerateOnActivation reports whether the four classic keys may be
// generated without an operator asking (FR-008, R15): the coordinator just
// became ready, no key row exists, and the controller reports an empty network.
// The node count is the condition that keeps a stick already carrying a network
// from having fresh keys invented for it, which would leave the hub unable to
// talk to the devices that network holds (FR-018). Long Range is never part of
// the automatic set, so nothing here consults it (FR-010). Any other state,
// the zero Activation included, reports false.
func ShouldGenerateOnActivation(a Activation) bool {
	return a.Connection == ConnectionReady &&
		a.KeyRevision == 0 &&
		a.Configured == 0 &&
		a.NodeCount == 0
}
