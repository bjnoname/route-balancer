package actor

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/bjnoname/route-balancer/internal/command"
)

func Send[M any](to *Mailbox[M], m M, id command.Action) command.Action {
	id.Write = func() (string, error) {
		select {
		case to.ch <- m:
			return "", nil
		default:
			return "", errors.New("the mailbox is full")
		}
	}
	id.OnFail = command.FailWarn

	return id
}

func Post[M any](ctx context.Context, to *Mailbox[M], m M) {
	select {
	case to.ch <- m:
	case <-ctx.Done():
	}
}

func Every[M any](ctx context.Context, to *Mailbox[M], every time.Duration, tick func() M) {
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				select {
				case to.ch <- tick():
				default:
					slog.Debug("Tick dropped: the mailbox is full")
				}
			}
		}
	}()
}
