// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// chatOwnerStore is the owner-only lookup the chat-image serving path uses
// instead of ChatByID, which reads the whole sealed transcript.
type chatOwnerStore interface {
	chatStore
	ChatOwner(ctx context.Context, chatID string) (userID string, err error)
}

var (
	_ chatOwnerStore = (*SQLStore)(nil)
	_ chatOwnerStore = (*MemoryChatStore)(nil)
)

// TestChatOwnerRoundTripAndMissing pins, on memory, sqlite and postgres alike,
// that ChatOwner returns the user_id the chat was created with, agrees with
// ChatByID, is ErrNotFound for a chat that never existed, and is ErrNotFound
// again once the chat is deleted.
func TestChatOwnerRoundTripAndMissing(t *testing.T) {
	forEachChatSaver(t, func(t *testing.T, saver chatSaver) {
		s, ok := saver.(chatOwnerStore)
		if !ok {
			t.Fatalf("%T has no ChatOwner", saver)
		}
		ctx := context.Background()
		now := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)

		if _, err := s.ChatOwner(ctx, "c_never"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("ChatOwner(never-created) = %v, want ErrNotFound", err)
		}
		if _, err := s.ChatOwner(ctx, ""); !errors.Is(err, ErrNotFound) {
			t.Fatalf("ChatOwner(empty id) = %v, want ErrNotFound", err)
		}

		chat := memChat("c_owner", 64, now)
		if err := s.CreateChat(ctx, chat); err != nil {
			t.Fatalf("CreateChat: %v", err)
		}
		got, err := s.ChatOwner(ctx, "c_owner")
		if err != nil {
			t.Fatalf("ChatOwner: %v", err)
		}
		if got != chat.UserID {
			t.Fatalf("ChatOwner = %q, want %q", got, chat.UserID)
		}
		row, err := s.ChatByID(ctx, "c_owner")
		if err != nil || row.UserID != got {
			t.Fatalf("ChatOwner %q disagrees with ChatByID %q (err %v)", got, row.UserID, err)
		}

		if err := s.DeleteChat(ctx, "c_owner"); err != nil {
			t.Fatalf("DeleteChat: %v", err)
		}
		if _, err := s.ChatOwner(ctx, "c_owner"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("ChatOwner(deleted) = %v, want ErrNotFound", err)
		}
	})
}
