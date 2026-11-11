// Package agenttest provides a scriptable agent.Agent for tests of the run manager and the
// HTTP API. A Script plays the CLI: it reads client inputs and emits events.
package agenttest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"

	"github.com/emreuyguc/native-agent-api/internal/agent"
)

// Input kinds delivered to a Script.
const (
	InputMessage   = "message"
	InputInterrupt = "interrupt"
	InputAnswer    = "answer"
	InputRespond   = "respond"
	InputClose     = "close"
)

// Input is one client action. The initial prompt arrives as the first InputMessage.
type Input struct {
	Kind   string
	Text   string
	Answer agent.PermissionAnswer
	// Respond fields.
	ID       string
	Response json.RawMessage
	Error    string
}

// Script plays the CLI until it returns; the returned error is what Wait reports.
type Script func(ctx context.Context, s *Session) error

// Agent is a fake agent. Started receives every request when non-nil.
type Agent struct {
	Script  Script
	Started chan agent.Request
	// StartErr makes Start fail.
	StartErr error

	mu   sync.Mutex
	last agent.Request
}

var _ agent.Agent = (*Agent)(nil)

// Info implements agent.Agent.
func (*Agent) Info() agent.Info {
	return agent.Info{
		Name: "fake", Title: "Fake CLI", Provider: "Test", Binary: "fake",
		Capabilities: []agent.Capability{}, LoginMethods: []string{"browser"}, TestedVersion: "1.0.0", Docs: "none",
	}
}

// Name implements agent.Agent.
func (*Agent) Name() string { return "fake" }

// ResultFields are the fake agent's fields of a turn result's provider section.
type ResultFields struct {
	Reason string `json:"reason"`
}

// ControlFields are the fake agent's fields of a control request's provider section.
type ControlFields struct {
	Subtype string `json:"subtype"`
}

// ProviderSections implements agent.SectionDescriber.
func (*Agent) ProviderSections() map[agent.Section]reflect.Type {
	return map[agent.Section]reflect.Type{
		agent.SectionResult:  reflect.TypeFor[ResultFields](),
		agent.SectionControl: reflect.TypeFor[ControlFields](),
	}
}

// Validate implements agent.Agent.
func (*Agent) Validate(req *agent.Request) error {
	if req.Effort != "" && req.Effort != "low" {
		return agent.Invalid("effort must be low")
	}

	return nil
}

// Version implements agent.Agent.
func (*Agent) Version(context.Context) (string, error) { return "1.2.3 (fake)\n", nil }

// AuthStatus implements agent.Agent.
func (*Agent) AuthStatus(context.Context) (agent.AuthStatus, error) {
	return agent.AuthStatus{LoggedIn: false, Provider: agent.AuthProvider{Provider: agent.NewProvider("fake", nil, json.RawMessage(`{"loggedIn":false}`))}}, nil
}

// Interactive implements agent.Agent.
func (*Agent) Interactive(context.Context, agent.IO, agent.InteractiveCommand) error { return nil }

// LastRequest returns the most recent request passed to Start.
func (a *Agent) LastRequest() agent.Request {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.last
}

// Start implements agent.Agent.
func (a *Agent) Start(ctx context.Context, req agent.Request, onEvent agent.EventFunc) (agent.Conversation, error) {
	if a.StartErr != nil {
		return nil, a.StartErr
	}
	a.mu.Lock()
	a.last = req
	a.mu.Unlock()
	if a.Started != nil {
		a.Started <- req
	}
	script := a.Script
	if script == nil {
		script = Echo
	}
	s := &Session{Request: req, nativeSessionID: req.SessionID, onEvent: onEvent, inputs: make(chan Input, 64), done: make(chan struct{})}
	s.inputs <- Input{Kind: InputMessage, Text: req.Prompt}
	go func() {
		s.err = script(ctx, s)
		close(s.done)
	}()

	return s, nil
}

// Session is the fake conversation; Scripts use Next and the emit helpers.
type Session struct {
	Request         agent.Request
	nativeSessionID string

	onEvent agent.EventFunc
	inputs  chan Input
	done    chan struct{}
	err     error

	mu     sync.Mutex
	closed bool
}

// NativeSessionID implements agent.NativeSession.
func (s *Session) NativeSessionID() string { return s.nativeSessionID }

// Next returns the next client input, or false when ctx ends.
func (s *Session) Next(ctx context.Context) (Input, bool) {
	select {
	case in := <-s.inputs:
		return in, true
	case <-ctx.Done():
		return Input{}, false
	}
}

func (s *Session) push(in Input) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return agent.ErrInputClosed
	}
	select {
	case <-s.done:
		return agent.ErrInputClosed
	case s.inputs <- in:
		return nil
	default:
		return errors.New("agenttest: input buffer full")
	}
}

// Send implements agent.Conversation.
func (s *Session) Send(text string) error { return s.push(Input{Kind: InputMessage, Text: text}) }

// Interrupt implements agent.Conversation.
func (s *Session) Interrupt() error { return s.push(Input{Kind: InputInterrupt}) }

// Answer implements agent.Conversation.
func (s *Session) Answer(answer agent.PermissionAnswer) error {
	return s.push(Input{Kind: InputAnswer, Answer: answer})
}

// Control implements agent.Conversation: it answers {"echo": request}; subtype "fail" is
// rejected.
func (s *Session) Control(_ context.Context, request json.RawMessage) (json.RawMessage, error) {
	var header struct {
		Subtype string `json:"subtype"`
	}
	if err := json.Unmarshal(request, &header); err != nil || header.Subtype == "" {
		return nil, agent.Invalid("control request must be a JSON object with a subtype")
	}
	if header.Subtype == "fail" {
		return nil, fmt.Errorf("%w: fail", agent.ErrControlRejected)
	}

	return json.Marshal(map[string]json.RawMessage{"echo": request})
}

// Respond implements agent.Conversation.
func (s *Session) Respond(id string, response json.RawMessage, errMessage string) error {
	return s.push(Input{Kind: InputRespond, ID: id, Response: response, Error: errMessage})
}

// CloseInput implements agent.Conversation.
func (s *Session) CloseInput() error {
	err := s.push(Input{Kind: InputClose})
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	if errors.Is(err, agent.ErrInputClosed) {
		return nil
	}

	return err
}

// Wait implements agent.Conversation.
func (s *Session) Wait() error {
	<-s.done
	return s.err
}

// Emit sends a native event.
func (s *Session) Emit(eventType, raw string) error {
	return s.onEvent(agent.Event{Type: eventType, Raw: json.RawMessage(raw)})
}

// Observe emits a native event carrying optional structured lifecycle data.
func (s *Session) Observe(raw string, observation agent.Observation) error {
	return s.onEvent(agent.Event{Type: "native", Raw: json.RawMessage(raw), Observation: &observation})
}

// StartTurn emits the first event of a turn.
func (s *Session) StartTurn() error {
	return s.onEvent(agent.Event{Type: "init", Raw: json.RawMessage(`{"type":"init"}`), TurnStarted: true})
}

// Finish emits the terminal event of a turn.
func (s *Session) Finish(text string, isError bool) error {
	return s.FinishWithUsage(text, isError, nil)
}

// FinishWithUsage emits a terminal event with explicit token counts (nil: unreported).
func (s *Session) FinishWithUsage(text string, isError bool, tokens *agent.Tokens) error {
	raw, _ := json.Marshal(map[string]any{"type": "result", "result": text, "is_error": isError})
	result := agent.OutcomeCompleted
	if isError {
		result = agent.OutcomeFailed
	}
	answer, failure := text, ""
	if isError {
		answer, failure = "", text
	}
	return s.onEvent(agent.Event{Type: "result", Raw: raw, Result: &agent.Result{
		Text: answer, Outcome: result, Error: failure, Tokens: tokens, Provider: resultProvider(raw),
	}})
}

// AgentTurn emits the terminal event of a turn the agent started on its own.
func (s *Session) AgentTurn(text string) error {
	raw, _ := json.Marshal(map[string]any{"type": "result", "result": text, "origin": map[string]string{"kind": "task-notification"}})
	return s.onEvent(agent.Event{Type: "result", Raw: raw, Result: &agent.Result{
		Text: text, Outcome: agent.OutcomeCompleted, Provider: resultProvider(raw), AgentInitiated: true,
	}})
}

func resultProvider(raw json.RawMessage) agent.ResultProvider {
	return agent.ResultProvider{Provider: agent.NewProvider("fake", ResultFields{Reason: "test"}, raw)}
}

// Ask emits a permission request of a kind (agent.Permission*).
func (s *Session) Ask(id, kind string) error {
	input := json.RawMessage(`{"file_path":"a.txt"}`)
	return s.onEvent(agent.Event{
		Type: "control_request", Raw: json.RawMessage(`{}`),
		Permission: &agent.PermissionRequest{
			ID: id, Kind: kind, Summary: "a.txt",
			Provider: agent.PermissionProvider{Provider: agent.NewProvider("fake", nil, input)},
		},
	})
}

// AskControl emits a generic control request.
func (s *Session) AskControl(id, subtype string) error {
	return s.onEvent(agent.Event{
		Type: "control_request", Raw: json.RawMessage(`{}`),
		Control: &agent.ControlRequest{ID: id, Provider: agent.ControlProvider{
			Provider: agent.NewProvider("fake", ControlFields{Subtype: subtype}, json.RawMessage(`{"subtype":"`+subtype+`"}`)),
		}},
	})
}

// Partial emits an incremental output event.
func (s *Session) Partial(text string) error {
	raw, _ := json.Marshal(map[string]string{"type": "stream_event", "delta": text})
	return s.onEvent(agent.Event{Type: "stream_event", Raw: raw, Partial: true})
}

// Echo answers every message with "echo: <text>" after one assistant event.
func Echo(ctx context.Context, s *Session) error {
	for {
		in, ok := s.Next(ctx)
		if !ok {
			return ctx.Err()
		}
		switch in.Kind {
		case InputClose:
			return nil
		case InputMessage:
			if err := s.StartTurn(); err != nil {
				return err
			}
			if err := s.Emit("assistant", `{"type":"assistant"}`); err != nil {
				return err
			}
			if err := s.Finish("echo: "+in.Text, false); err != nil {
				return err
			}
		}
	}
}

// Blocking starts a turn and then waits: an interrupt ends the turn with an error result,
// the context ends the process. Release (when non-nil) finishes the turn successfully.
func Blocking(release <-chan struct{}) Script {
	return func(ctx context.Context, s *Session) error {
		turnOpen := false
		for {
			select {
			case <-release:
				release = nil
				if turnOpen {
					turnOpen = false
					if err := s.Finish("released", false); err != nil {
						return err
					}
				}
				continue
			case in := <-s.inputs:
				switch in.Kind {
				case InputClose:
					return nil
				case InputMessage:
					if err := s.StartTurn(); err != nil {
						return err
					}
					turnOpen = true
				case InputInterrupt:
					if turnOpen {
						turnOpen = false
						if err := s.Finish("[Request interrupted by user]", true); err != nil {
							return err
						}
					}
				}
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

// Hook sends a hook_callback control request per message and reports the client's answer.
func Hook(ctx context.Context, s *Session) error {
	for {
		in, ok := s.Next(ctx)
		if !ok {
			return ctx.Err()
		}
		switch in.Kind {
		case InputClose:
			return nil
		case InputMessage:
			if err := s.StartTurn(); err != nil {
				return err
			}
			if err := s.AskControl("hook-1", "hook_callback"); err != nil {
				return err
			}
		case InputRespond:
			if err := s.Finish(fmt.Sprintf("id=%s response=%s error=%s", in.ID, string(in.Response), in.Error), false); err != nil {
				return err
			}
		}
	}
}

// Streaming emits three partial events before each complete message.
func Streaming(ctx context.Context, s *Session) error {
	for {
		in, ok := s.Next(ctx)
		if !ok {
			return ctx.Err()
		}
		switch in.Kind {
		case InputClose:
			return nil
		case InputMessage:
			if err := s.StartTurn(); err != nil {
				return err
			}
			for _, part := range []string{"ec", "ho", "!"} {
				if err := s.Partial(part); err != nil {
					return err
				}
			}
			if err := s.Emit("assistant", `{"type":"assistant"}`); err != nil {
				return err
			}
			if err := s.Finish("echo!", false); err != nil {
				return err
			}
		}
	}
}

// Permission asks for "Write" once per message and reports the decision as the result.
func Permission(ctx context.Context, s *Session) error {
	for {
		in, ok := s.Next(ctx)
		if !ok {
			return ctx.Err()
		}
		switch in.Kind {
		case InputClose:
			return nil
		case InputMessage:
			if err := s.StartTurn(); err != nil {
				return err
			}
			if err := s.Ask("perm-1", agent.PermissionFileChange); err != nil {
				return err
			}
		case InputAnswer:
			if err := s.Finish(fmt.Sprintf("allow=%t message=%s", in.Answer.Allow, in.Answer.Message), false); err != nil {
				return err
			}
		}
	}
}
