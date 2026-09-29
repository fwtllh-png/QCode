package web

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/fwtllh-png/QCode/internal/security/credential"
)

var (
	errCredentialRotationNotActivated = errors.New("staged credential rotation was not activated")
	errCredentialRotationSettled      = errors.New("credential rotation was rolled back")
)

type credentialRotationPhase int

const (
	// credentialRotationIdle carries the current credential; nothing is staged.
	credentialRotationIdle credentialRotationPhase = iota
	credentialRotationStaged
	credentialRotationActivated
	credentialRotationCommitted
	credentialRotationRolledBack
)

// credentialRotation is the Web host's one stage → activate → commit
// transaction over a credential.Control. Phases only move forward; Commit is
// refused until Activate succeeds, and Rollback restores the previous
// reference from staged or activated and is a no-op once committed. A nil or
// idle rotation is a no-op transaction that still names the effective
// credential, so callers never branch on whether a secret was submitted.
type credentialRotation struct {
	control   *credential.Control
	reference credential.Reference
	previous  credential.Reference
	phase     credentialRotationPhase
}

// stageCredentialRotation stages secret in control's keyring namespace. An
// empty secret yields an idle rotation over current.
func stageCredentialRotation(
	ctx context.Context,
	control *credential.Control,
	current credential.Reference,
	secret string,
) (*credentialRotation, error) {
	rotation := &credentialRotation{control: control, reference: current}
	if secret == "" {
		return rotation, nil
	}
	status, err := credential.New(
		current,
		credential.WithControl(control),
		credential.WithLiveReload(),
	).StageKeyring(ctx, secret)
	if err != nil {
		return nil, fmt.Errorf("store setup credential: %w", err)
	}
	rotation.previous = current
	rotation.reference = status.Reference
	rotation.phase = credentialRotationStaged
	return rotation, nil
}

func (r *credentialRotation) Control() *credential.Control {
	if r == nil {
		return nil
	}
	return r.control
}

// Reference is the credential the rotation makes effective: the staged one
// while pending or committed, otherwise the current one.
func (r *credentialRotation) Reference() credential.Reference {
	if r == nil {
		return credential.Reference{}
	}
	if r.phase == credentialRotationRolledBack {
		return r.previous
	}
	return r.reference
}

// Pending reports a staged or activated rotation that still needs Commit or
// Rollback.
func (r *credentialRotation) Pending() bool {
	return r != nil &&
		(r.phase == credentialRotationStaged || r.phase == credentialRotationActivated)
}

// bindTo records a pending rotation's reference on its owning connection;
// connectionID "" names the active connection. Only the owner changes, so a
// key staged for one connection never becomes another connection's credential.
func (r *credentialRotation) bindTo(selection webSetupSelection, connectionID string) bool {
	if !r.Pending() {
		return false
	}
	target := selection.Active()
	if connectionID != "" {
		target = selection.Connection(connectionID)
	}
	if target == nil {
		return false
	}
	value := r.reference
	target.Credential = &value
	return true
}

func (r *credentialRotation) Activate() error {
	if r == nil {
		return nil
	}
	switch r.phase {
	case credentialRotationStaged:
		if err := r.control.Activate(context.Background(), r.reference); err != nil {
			return err
		}
		r.phase = credentialRotationActivated
		return nil
	case credentialRotationRolledBack:
		return errCredentialRotationSettled
	default:
		return nil
	}
}

func (r *credentialRotation) Commit() error {
	if r == nil {
		return nil
	}
	switch r.phase {
	case credentialRotationStaged:
		return errCredentialRotationNotActivated
	case credentialRotationActivated:
		if err := r.control.Commit(context.Background(), r.reference); err != nil {
			return err
		}
		r.phase = credentialRotationCommitted
		return nil
	case credentialRotationRolledBack:
		return errCredentialRotationSettled
	default:
		return nil
	}
}

func (r *credentialRotation) Rollback() error {
	if !r.Pending() {
		return nil
	}
	if err := r.control.Restore(context.Background(), r.reference, r.previous); err != nil {
		return err
	}
	r.phase = credentialRotationRolledBack
	return nil
}

// settle ends the transaction: a failed change rolls back into *resultErr; a
// successful one commits.
func (r *credentialRotation) settle(resultErr *error, stderr io.Writer) {
	if *resultErr != nil {
		*resultErr = errors.Join(*resultErr, r.Rollback())
		return
	}
	r.commitOrReport(stderr)
}

// commitOrReport commits after the change has succeeded. The new credential
// is already live once activated, so a commit failure only leaves recovery
// cleanup behind and is reported rather than failing the change.
func (r *credentialRotation) commitOrReport(stderr io.Writer) {
	if err := r.Commit(); err != nil && stderr != nil {
		_, _ = fmt.Fprintf(stderr, "qcode: finalize credential rotation: %v\n", err)
	}
}
