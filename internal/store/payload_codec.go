package store

import (
	"encoding/json"
	"errors"
	"fmt"

	"resy-snipe/internal/domain"
)

// SlotPayloadKind tags the wire-format discriminator written when a
// SlotPayload is persisted. Adding a new domain.SlotPayload variant
// requires:
//  1. defining the type and isSlotPayload() in internal/domain;
//  2. adding a kind constant here;
//  3. extending both switches in MarshalSlotPayload and
//     UnmarshalSlotPayload below.
//
// The default branches on each switch turn an out-of-date codec into a
// loud, immediate runtime error rather than a silent data drop.
type SlotPayloadKind string

const (
	SlotPayloadKindResy SlotPayloadKind = "resy"
)

// slotPayloadEnvelope is the persisted JSON shape: a single
// discriminator plus the type-specific body. json.RawMessage is the
// reason this codec lives in store/ rather than domain/ — the project
// gates ban RawMessage in any other package.
type slotPayloadEnvelope struct {
	Kind    SlotPayloadKind `json:"kind"`
	Payload json.RawMessage `json:"payload"`
}

// MarshalSlotPayload encodes a typed SlotPayload to the wire bytes the
// store will write. Engine and provider code never call this; only the
// store boundary does.
func MarshalSlotPayload(p domain.SlotPayload) ([]byte, error) {
	if p == nil {
		return nil, errors.New("MarshalSlotPayload: nil payload")
	}
	switch v := p.(type) {
	case domain.ResySlotPayload:
		body, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("MarshalSlotPayload resy body: %w", err)
		}
		return json.Marshal(slotPayloadEnvelope{
			Kind:    SlotPayloadKindResy,
			Payload: body,
		})
	default:
		return nil, fmt.Errorf("MarshalSlotPayload: unknown variant %T", p)
	}
}

// UnmarshalSlotPayload decodes wire bytes back into a typed SlotPayload.
func UnmarshalSlotPayload(data []byte) (domain.SlotPayload, error) {
	var env slotPayloadEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("UnmarshalSlotPayload envelope: %w", err)
	}
	switch env.Kind {
	case SlotPayloadKindResy:
		var p domain.ResySlotPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			return nil, fmt.Errorf("UnmarshalSlotPayload resy: %w", err)
		}
		return p, nil
	case "":
		return nil, errors.New("UnmarshalSlotPayload: missing kind discriminator")
	default:
		return nil, fmt.Errorf("UnmarshalSlotPayload: unknown kind %q", env.Kind)
	}
}
