package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/arapan-gabriel/email-verifier/internal/relay"
)

// Relay is the engine behind POST /send, declared here in the package that
// calls it (ENGINEERING-STANDARDS §2).
type Relay interface {
	Accept(ctx context.Context, m relay.Message) (string, error)
}

type sendRequest struct {
	From    string         `json:"from"`
	To      string         `json:"to"`
	Subject string         `json:"subject"`
	Text    string         `json:"text,omitempty"`
	HTML    string         `json:"html,omitempty"`
	Headers []relay.Header `json:"headers,omitempty"`
}

type sendResponse struct {
	MessageID string    `json:"message_id"`
	QueuedAt  time.Time `json:"queued_at"`
}

// handleSend accepts one transactional message and answers once it is durable
// — not once it is delivered.
//
// The distinction is the contract: a caller that is told `202` may stop holding
// the message, so this route must not answer until a restart could not lose it.
// Delivery happens on the drain, and what a receiving server eventually says
// about it comes back as a bounce (plan 015), not as this response.
func handleSend(r Relay) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		var body sendRequest
		dec := json.NewDecoder(http.MaxBytesReader(w, req.Body, maxSendBytes))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil {
			WriteError(w, http.StatusBadRequest, "bad_request", "malformed JSON body: "+err.Error())
			return
		}

		id, err := r.Accept(req.Context(), relay.Message{
			From: body.From, To: body.To, Subject: body.Subject,
			Text: body.Text, HTML: body.HTML, Extra: body.Headers,
		})
		switch {
		case err == nil:
		case errors.Is(err, relay.ErrNotSending):
			// The node must not send at all right now — a listed IP, or a
			// suppression list it cannot vouch for. That is not the caller's
			// fault and not a permanent refusal of this message, so it is a
			// 503 the caller may retry, never a 400 that invites it to give up.
			WriteError(w, http.StatusServiceUnavailable, "not_sending", err.Error())
			return
		default:
			// Everything else is the request: a malformed address, a reserved
			// header, a suppressed recipient. A verdict about the message,
			// which the caller can act on without retrying.
			WriteError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, sendResponse{MessageID: id, QueuedAt: time.Now().UTC()})
	}
}

// maxSendBytes caps a request body. Transactional mail is short; a body this
// size is a mistake or an attack, and either way it should not reach the queue.
const maxSendBytes = 2 << 20
