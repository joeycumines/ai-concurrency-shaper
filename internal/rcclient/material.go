// Copyright (C) 2026 Joseph Cumines
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package rcclient

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
)

// DecodeMaterial resolves flag-supplied key material. Two forms are accepted:
// "file:PATH" reads the file at PATH, and "base64:DATA" decodes DATA as
// standard base64. Any other form is an error — the explicit prefix states
// the source instead of guessing it from content. An empty value yields nil,
// nil (material unset).
//
// A configured value that decodes to zero bytes is an error, never an empty
// slice: callers distinguish "unset" from "set" by nil, and a silently empty
// CA or certificate makes every len>0 guard downstream skip the material it
// was configured with — the handshake would then trust the system roots or
// omit a configured client certificate while the operator's flags say
// otherwise.
func DecodeMaterial(value string) ([]byte, error) {
	if value == "" {
		return nil, nil
	}
	if path, ok := strings.CutPrefix(value, "file:"); ok {
		if path == "" {
			return nil, errors.New("rcclient: empty path in file: material")
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("rcclient: reading key material: %w", err)
		}
		if len(b) == 0 {
			return nil, errors.New("rcclient: key material is empty")
		}
		return b, nil
	}
	if data, ok := strings.CutPrefix(value, "base64:"); ok {
		b, err := base64.StdEncoding.DecodeString(data)
		if err != nil {
			return nil, fmt.Errorf("rcclient: decoding base64 key material: %w", err)
		}
		if len(b) == 0 {
			return nil, errors.New("rcclient: key material is empty")
		}
		return b, nil
	}
	// Never echo the material back: an operator who pastes a raw private key
	// without the file:/base64: prefix would otherwise leak it into the error
	// (and thus the journal/log). Report the size only.
	return nil, fmt.Errorf("rcclient: key material must use a file: or base64: prefix (got %d bytes)", len(value))
}
