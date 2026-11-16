package doubao

import (
	"strings"
	"workbuddy2api/internal/inference"
	"workbuddy2api/internal/providers/doubao/stream"
)

// Progress preserves append-only transport semantics across atomic snapshots.
// If the upstream retracts already delivered content, terminate explicitly;
// the caller must discard the incomplete answer instead of receiving a false
// successful concatenation. Buffered completion can still accept revisions.
type textProgress struct {
	text, delivered string
	emit            func(string) error
}

func (p *textProgress) apply(snapshot stream.TextSnapshot) error {
	p.text = snapshot.Text
	if p.emit == nil || !snapshot.Acknowledged {
		return nil
	}
	if !strings.HasPrefix(snapshot.Text, p.delivered) {
		return inference.ErrStreamRevised
	}
	delta := snapshot.Text[len(p.delivered):]
	if delta == "" {
		return nil
	}
	if err := p.emit(delta); err != nil {
		return err
	}
	p.delivered = snapshot.Text
	return nil
}
