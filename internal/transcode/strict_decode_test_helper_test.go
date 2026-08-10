package transcode

// Test-only strict decode helper. Production decode entry points use
// wire.Decode (internal/transcode/wire); this helper keeps the test
// fixtures' strict unknown-field and trailing-value semantics without the
// wire layer's duplicate-key and illegal-null rejections, which some
// historical fixtures do not satisfy.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// strictDecode decodes exactly one JSON value into dst with unknown fields
// rejected and trailing values rejected.
func strictDecode(data []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		return err
	}

	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}
