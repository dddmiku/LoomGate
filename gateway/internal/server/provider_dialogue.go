package server

import (
	"context"
	"workbuddy2api/internal/inference"
)

func providerDialogueSafely(ctx context.Context, p inference.DialogueProvider, q inference.Dialogue, emit func(string) error) (result inference.Result, err error) {
	defer func() {
		if recover() != nil {
			result = inference.Result{Attempted: true}
			err = inference.ErrStreamContract
		}
	}()
	return p.CompleteDialogue(ctx, q, emit)
}
