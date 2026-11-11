package httpapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"path"
	"strconv"

	"github.com/emreuyguc/native-agent-api/internal/ids"
	"github.com/emreuyguc/native-agent-api/internal/permission"
	"github.com/emreuyguc/native-agent-api/internal/session"
	"github.com/emreuyguc/native-agent-api/internal/workspace"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
)

type WorkspacePath struct {
	WorkspaceID string `path:"workspace_id" doc:"32 lowercase hex characters." example:"0123456789abcdef0123456789abcdef"`
}

type FilePath struct {
	WorkspacePath

	Path string `path:"path" doc:"Slash-separated path inside the workspace." example:"src/main.go"`
}

// createWorkspace creates an empty managed workspace.
func (s *Server) createWorkspace(ctx context.Context, _ *struct{}) (*body[workspace.Info], error) {
	id, err := s.workspaces.Create()
	if err != nil {
		s.log.ErrorContext(ctx, "create workspace", "err", err)
		return nil, fail(http.StatusInternalServerError, codeWorkspaceCreateFailed, "Unable to create workspace.")
	}
	s.log.InfoContext(ctx, "workspace created", "workspace_id", id)
	info, err := s.workspaces.Info(id)
	if err != nil {
		return nil, s.workspaceError(ctx, err)
	}

	return reply(info), nil
}

// withLastUsed adds the gateway's session activity to workspace descriptions.
func (s *Server) withLastUsed(infos ...workspace.Info) []workspace.Info {
	last := s.sessions.LastUsed()
	for i := range infos {
		if used, ok := last[infos[i].ID]; ok {
			infos[i].Times.LastUsedAt = &used
		}
	}

	return infos
}

type attachWorkspaceInput struct {
	Body struct {
		Path string `json:"path" minLength:"1" doc:"Absolute directory inside NATIVE_AGENT_ALLOWED_ROOTS."`
	}
}

// attachWorkspace attaches an existing directory inside NATIVE_AGENT_ALLOWED_ROOTS.
func (s *Server) attachWorkspace(ctx context.Context, in *attachWorkspaceInput) (*body[workspace.Info], error) {
	info, err := s.workspaces.AttachHostDirectory(in.Body.Path)
	switch {
	case errors.Is(err, workspace.ErrAttachmentDisabled):
		return nil, fail(http.StatusForbidden, codeAttachmentDisabled, "Host-directory attachments need NATIVE_AGENT_ALLOWED_ROOTS.")
	case errors.Is(err, workspace.ErrInvalidPath):
		s.log.WarnContext(ctx, "workspace attachment refused", "path", in.Body.Path, "err", err)
		return nil, fail(http.StatusForbidden, codePathNotAllowed, "The path is not a directory inside an allowed root.")
	case err != nil:
		s.log.ErrorContext(ctx, "attach host directory", "err", err)
		return nil, fail(http.StatusInternalServerError, codeWorkspaceAttachFailed, "Unable to attach host directory.")
	}
	s.log.InfoContext(ctx, "host directory attached", "workspace_id", info.ID, "path", info.Attachment.Path)

	return reply(s.withLastUsed(info)[0]), nil
}

type workspaceListInput struct{ PageInput }

func (s *Server) listWorkspaces(ctx context.Context, input *workspaceListInput) (*body[list[workspace.Info]], error) {
	infos, err := s.workspaces.Workspaces()
	if err != nil {
		s.log.ErrorContext(ctx, "list workspaces", "err", err)
		return nil, fail(http.StatusInternalServerError, codeWorkspaceListFailed, "Unable to list workspaces.")
	}

	return page(s.withLastUsed(infos...), input.PageInput, func(item workspace.Info) string { return item.ID })
}

func (s *Server) getWorkspace(ctx context.Context, in *WorkspacePath) (*body[workspace.Info], error) {
	info, err := s.workspaces.Info(in.WorkspaceID)
	if err != nil {
		return nil, s.workspaceError(ctx, err)
	}

	return reply(s.withLastUsed(info)[0]), nil
}

// removeWorkspace deletes a managed workspace or detaches an attached directory; storage
// names the kind the operation is for, so a client never deletes what it meant to detach.
func (s *Server) removeWorkspace(storage, action string) func(context.Context, *WorkspacePath) (*struct{}, error) {
	return func(ctx context.Context, in *WorkspacePath) (*struct{}, error) {
		id := in.WorkspaceID
		info, err := s.workspaces.Info(id)
		if err != nil {
			return nil, s.workspaceError(ctx, err)
		}
		if info.Storage != storage {
			hint := "detach"
			if info.Storage == "managed" {
				hint = "delete"
			}
			return nil, fail(http.StatusConflict, codeWrongWorkspaceStorage,
				"The workspace has "+info.Storage+" storage; use /v1/workspaces/{workspace_id}/"+hint+".")
		}
		err = s.sessions.RemoveIfIdle(id, func() error { return s.workspaces.Remove(id) })
		if errors.Is(err, session.ErrBusy) {
			s.log.WarnContext(ctx, "workspace "+action+" refused: active session", "workspace_id", id)
			return nil, fail(http.StatusConflict, codeWorkspaceBusy, "Workspace has an active session.")
		}
		if err != nil {
			return nil, s.workspaceError(ctx, err)
		}
		s.rules.Clear(id)
		s.log.InfoContext(ctx, "workspace "+action, "workspace_id", id)

		return nil, nil
	}
}

type SessionPath struct {
	SessionID string `path:"session_id" format:"uuid"`
}

type sessionListInput struct {
	PageInput
	WorkspaceID string `query:"workspace_id" doc:"Optional workspace ID filter."`
	State       string `query:"state" enum:"idle,active,completed,orphaned,unavailable" doc:"Optional lifecycle-state filter."`
}

func (s *Server) listSessions(_ context.Context, in *sessionListInput) (*body[list[session.View]], error) {
	if in.WorkspaceID != "" && !ids.ValidWorkspaceID(in.WorkspaceID) {
		return nil, fail(http.StatusUnprocessableEntity, codeInvalidWorkspace, "workspace_id is invalid.")
	}

	all := s.sessions.List(in.WorkspaceID)
	filtered := make([]session.View, 0, len(all))
	for index := range all {
		if info := &all[index]; in.State == "" || info.State == in.State {
			filtered = append(filtered, info.View())
		}
	}
	return page(filtered, in.PageInput, func(item session.View) string { return item.SessionID })
}

func (s *Server) getSession(ctx context.Context, in *SessionPath) (*body[session.View], error) {
	info, err := s.sessions.Get(in.SessionID)
	if err != nil {
		return nil, s.sessionError(ctx, err)
	}

	return reply(info.View()), nil
}

func (s *Server) deleteSession(ctx context.Context, in *SessionPath) (*struct{}, error) {
	if err := s.sessions.Delete(in.SessionID); err != nil {
		return nil, s.sessionError(ctx, err)
	}
	s.rules.ClearSession(in.SessionID)
	s.log.InfoContext(ctx, "gateway session metadata deleted", "session_id", in.SessionID, "native_transcript", "retained")

	return nil, nil
}

func (s *Server) sessionError(ctx context.Context, err error) error {
	s.log.DebugContext(ctx, "session request rejected", "err", err)
	switch {
	case errors.Is(err, session.ErrNotFound):
		return fail(http.StatusNotFound, codeSessionNotFound, err.Error())
	case errors.Is(err, session.ErrBusy):
		return fail(http.StatusConflict, codeSessionBusy, "An active session cannot be deleted.")
	case errors.Is(err, session.ErrInvalid):
		return fail(http.StatusUnprocessableEntity, codeInvalidSession, err.Error())
	case errors.Is(err, session.ErrNotStarted):
		return fail(http.StatusConflict, codeSessionNotStarted, "The session has no native conversation to fork yet.")
	case errors.Is(err, session.ErrExists):
		return fail(http.StatusConflict, codeSessionExists, err.Error())
	default:
		s.log.ErrorContext(ctx, "session operation failed", "err", err)
		return fail(http.StatusInternalServerError, codeSessionError, "Session operation failed.")
	}
}

type fileListInput struct {
	WorkspacePath
	PageInput
}

func (s *Server) listFiles(ctx context.Context, in *fileListInput) (*body[list[workspace.Entry]], error) {
	entries, err := s.workspaces.List(in.WorkspaceID)
	if err != nil {
		return nil, s.workspaceError(ctx, err)
	}
	return page(entries, in.PageInput, func(item workspace.Entry) string { return item.Path })
}

// uploadInput streams the request body straight to the workspace store: Huma does not read
// binary bodies, the resolver hands over the reader.
type uploadInput struct {
	FilePath

	body   io.Reader
	length int64
}

func (u *uploadInput) Resolve(ctx huma.Context) []error {
	u.body = ctx.BodyReader()
	u.length, _ = strconv.ParseInt(ctx.Header("Content-Length"), 10, 64)
	return nil
}

func (s *Server) uploadFile(ctx context.Context, in *uploadInput) (*body[workspace.UploadedFile], error) {
	limit := s.cfg.Limits.MaxFileBytes
	if in.length > limit {
		return nil, fail(http.StatusRequestEntityTooLarge, codeFileTooLarge, workspace.ErrFileTooLarge.Error())
	}
	// One byte over the limit lets the store detect oversized bodies without Content-Length.
	file, err := s.workspaces.Upload(in.WorkspaceID, in.Path, io.LimitReader(in.body, limit+1))
	if err != nil {
		return nil, s.workspaceError(ctx, err)
	}
	s.log.InfoContext(ctx, "file uploaded", "workspace_id", in.WorkspaceID, "path", file.Path, "size", file.Size, "sha256", file.SHA256)

	return reply(file), nil
}

func (s *Server) downloadFile(ctx context.Context, in *FilePath) (*huma.StreamResponse, error) {
	file, entry, err := s.workspaces.Open(in.WorkspaceID, in.Path)
	if err != nil {
		return nil, s.workspaceError(ctx, err)
	}

	return &huma.StreamResponse{Body: func(hc huma.Context) {
		defer file.Close()
		r, w := humago.Unwrap(hc)
		name := path.Base(entry.Path)
		w.Header().Set("Content-Type", entry.MediaType)
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
		http.ServeContent(w, r, name, entry.Modified, file) // Handles Range and conditional requests.
	}}, nil
}

type pathInput struct {
	WorkspacePath
	Body struct {
		Path string `json:"path" minLength:"1" doc:"Slash-separated path inside the workspace." example:"output/old"`
	}
}

// deleteFile deletes a file or a directory tree.
func (s *Server) deleteFile(ctx context.Context, in *pathInput) (*struct{}, error) {
	if err := s.workspaces.Delete(in.WorkspaceID, in.Body.Path); err != nil {
		return nil, s.workspaceError(ctx, err)
	}
	s.log.InfoContext(ctx, "workspace path deleted", "workspace_id", in.WorkspaceID, "path", in.Body.Path)

	return nil, nil
}

type mkdirInput struct {
	WorkspacePath
	Body struct {
		Path     string                   `json:"path"     minLength:"1" doc:"Directory to create; its parent must exist." example:"archive"`
		Conflict workspace.ConflictPolicy `json:"conflict" enum:"error,replace" doc:"error: 409 when the path exists; replace: replace it with an empty directory."`
	}
}

func (s *Server) makeDirectory(ctx context.Context, in *mkdirInput) (*body[workspace.Entry], error) {
	entry, err := s.workspaces.CreateDirectory(in.WorkspaceID, in.Body.Path, in.Body.Conflict)
	if err != nil {
		return nil, s.workspaceError(ctx, err)
	}
	s.log.InfoContext(ctx, "workspace directory created", "workspace_id", in.WorkspaceID, "path", entry.Path, "conflict", in.Body.Conflict)

	return reply(entry), nil
}

type transferInput struct {
	WorkspacePath
	Body struct {
		Source      string                   `json:"source"      minLength:"1" doc:"Existing path inside the workspace." example:"output/result.txt"`
		Destination string                   `json:"destination" minLength:"1" doc:"New path; its parent must exist." example:"archive/result.txt"`
		Conflict    workspace.ConflictPolicy `json:"conflict"    enum:"error,replace" doc:"error: 409 when the destination exists; replace: replace it (rolled back on failure)."`
	}
}

// transfer returns the copy or move (rename) handler.
func (s *Server) transfer(name string, operation func(id, source, destination string, conflict workspace.ConflictPolicy) (workspace.Entry, error)) func(context.Context, *transferInput) (*body[workspace.Entry], error) {
	return func(ctx context.Context, in *transferInput) (*body[workspace.Entry], error) {
		entry, err := operation(in.WorkspaceID, in.Body.Source, in.Body.Destination, in.Body.Conflict)
		if err != nil {
			return nil, s.workspaceError(ctx, err)
		}
		s.log.InfoContext(ctx, "workspace file "+name, "workspace_id", in.WorkspaceID,
			"source", in.Body.Source, "destination", entry.Path, "conflict", in.Body.Conflict)

		return reply(entry), nil
	}
}

// workspaceError maps workspace sentinel errors to API errors.
func (s *Server) workspaceError(ctx context.Context, err error) error {
	s.log.DebugContext(ctx, "workspace request rejected", "err", err)
	switch {
	case errors.Is(err, workspace.ErrInvalidID), errors.Is(err, workspace.ErrNotFound):
		return fail(http.StatusNotFound, codeNotFound, err.Error())
	case errors.Is(err, workspace.ErrInvalidPath), errors.Is(err, workspace.ErrIsDirectory):
		return fail(http.StatusUnprocessableEntity, codeInvalidPath, err.Error())
	case errors.Is(err, workspace.ErrInvalidOperation):
		return fail(http.StatusUnprocessableEntity, codeInvalidOperation, err.Error())
	case errors.Is(err, workspace.ErrPathConflict):
		return fail(http.StatusConflict, codePathConflict, err.Error())
	case errors.Is(err, workspace.ErrFileTooLarge):
		return fail(http.StatusRequestEntityTooLarge, codeFileTooLarge, err.Error())
	case errors.Is(err, workspace.ErrTooManyEntries):
		return fail(http.StatusUnprocessableEntity, codeTooManyEntries, err.Error())
	default:
		s.log.ErrorContext(ctx, "workspace operation failed", slog.Any("err", err))
		return fail(http.StatusInternalServerError, codeWorkspaceError, "Workspace operation failed.")
	}
}

func (s *Server) listPermissionRules(ctx context.Context, in *WorkspacePath) (*body[permission.Rules], error) {
	if _, err := s.workspaces.Dir(in.WorkspaceID); err != nil {
		return nil, s.workspaceError(ctx, err)
	}

	return reply(s.rules.Rules(in.WorkspaceID)), nil
}

func (s *Server) clearPermissionRules(ctx context.Context, in *WorkspacePath) (*struct{}, error) {
	if _, err := s.workspaces.Dir(in.WorkspaceID); err != nil {
		return nil, s.workspaceError(ctx, err)
	}
	s.rules.Clear(in.WorkspaceID)
	s.log.InfoContext(ctx, "permission rules cleared", "workspace_id", in.WorkspaceID)

	return nil, nil
}
