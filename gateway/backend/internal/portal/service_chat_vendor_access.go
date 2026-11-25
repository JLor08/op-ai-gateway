// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"op-ai-gateway/internal/auth"
)

// explicitNoVendorAccess is what an explicit "no providers" chat policy is
// stored as. The strict-default encoding is "" in the token column, but for the
// chat session "" means unset (all accounts and groups), so the zero policy needs
// its own non-empty spelling.
const explicitNoVendorAccess = `{"all":false}`

// EncodeChatSessionVendorAccess validates d for the token-less portal chat of
// userID -- exactly as a user token's policy is validated, against what that user
// can reach -- and returns the value to store in users.chat_vendor_provider_access.
// A nil or empty policy is the user's explicit choice of no providers and encodes
// to {"all":false}, never "" (which would read back as unset). Errors are
// ErrTokenVendorAccessInvalid / ErrTokenVendorAccessConflict, or raw on a store
// failure.
func (s *Service) EncodeChatSessionVendorAccess(ctx context.Context, userID string, d *VendorAccessDTO) (string, error) {
	if err := s.validateVendorAccess(ctx, userTokenPrincipal(auth.Token{UserID: userID}), d); err != nil {
		return "", err
	}
	if encoded := encodeVendorAccessDTO(d); encoded != "" {
		return encoded, nil
	}
	return explicitNoVendorAccess, nil
}
