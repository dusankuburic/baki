package database

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"pad-analyzer/internal/storage/interfaces"
)

// TestAzureStatements_AllCarryOwnerPredicate: every registry statement, for
// both owner kinds, names its owner explicitly. /api/chat/* runs without
// the RLS transaction, where the policies allow everything, so a statement
// without the predicate would read or write across owners.
func TestAzureStatements_AllCarryOwnerPredicate(t *testing.T) {
	for kind, st := range azureSQL {
		ownerCol := map[interfaces.AzureOwnerKind]string{interfaces.AzureOwnerUser: "user_id", interfaces.AzureOwnerOrg: "org_id"}[kind]
		v := reflect.ValueOf(*st)
		for i := range v.NumField() {
			name, sql := v.Type().Field(i).Name, v.Field(i).String()
			if sql == "" {
				if kind == interfaces.AzureOwnerUser && (name == "clearDefault" || name == "setDefault") {
					continue // users have no default for members
				}
				t.Errorf("%s %s is empty", kind, name)
				continue
			}
			if strings.ContainsAny(sql, "{}") {
				t.Errorf("%s %s has an unreplaced placeholder:\n%s", kind, name, sql)
			}
			if strings.HasPrefix(strings.TrimSpace(sql), "INSERT") {
				// An insert names its owner as the first value ($1) and
				// any limit subquery is owner-bounded too.
				if !strings.Contains(sql, "("+ownerCol+", ") || !strings.Contains(sql, "$1") {
					t.Errorf("%s %s does not insert the owner:\n%s", kind, name, sql)
				}
				if strings.Contains(sql, "count(*)") && !strings.Contains(sql, "WHERE "+ownerCol+" = $1") {
					t.Errorf("%s %s counts across owners:\n%s", kind, name, sql)
				}
				continue
			}
			if strings.Count(sql, ownerCol+" = $1") < strings.Count(sql, "WHERE") {
				t.Errorf("%s %s has a WHERE clause without the owner predicate %s = $1:\n%s", kind, name, ownerCol, sql)
			}
			if !strings.Contains(sql, ownerCol+" = $1") {
				t.Errorf("%s %s lacks the owner predicate %s = $1:\n%s", kind, name, ownerCol, sql)
			}
			other := map[string]string{"user_id": "org_", "org_id": "user_"}[ownerCol]
			if strings.Contains(sql, other+"azure_") {
				t.Errorf("%s %s touches the other owner kind's tables:\n%s", kind, name, sql)
			}
		}
	}
}

// TestMapAzureRegistryErr maps each constraint violation to its typed
// error by constraint name, for both table sets, and leaves other errors
// wrapped.
func TestMapAzureRegistryErr(t *testing.T) {
	for _, tc := range []struct {
		code, constraint string
		want             error
	}{
		{pgErrUniqueViolation, "user_azure_connections_pkey", interfaces.ErrAzureDuplicateConnectionID},
		{pgErrUniqueViolation, "org_azure_connections_name_idx", interfaces.ErrAzureDuplicateConnectionName},
		{pgErrUniqueViolation, "org_azure_models_pkey", interfaces.ErrAzureDuplicateModelID},
		{pgErrUniqueViolation, "user_azure_models_deployment_idx", interfaces.ErrAzureDuplicateModel},
		{pgErrUniqueViolation, "user_azure_models_kb_idx", interfaces.ErrAzureDuplicateDesignation},
		{pgErrUniqueViolation, "org_azure_models_default_idx", interfaces.ErrAzureDuplicateDesignation},
		{pgErrCheckViolation, "org_azure_models_kb_check", interfaces.ErrAzureInvalidDesignation},
		{pgErrCheckViolation, "org_azure_models_default_check", interfaces.ErrAzureInvalidDesignation},
		{pgErrCheckViolation, "user_azure_models_limits_check", interfaces.ErrAzureRegistryInvalid},
		{pgErrForeignKeyViolation, "user_azure_models_connection_fk", interfaces.ErrAzureConnectionNotFound},
		{pgErrForeignKeyViolation, "user_azure_connections_user_fk", interfaces.ErrAzureOwnerNotFound},
		{pgErrForeignKeyViolation, "org_azure_registry_state_org_fk", interfaces.ErrAzureOwnerNotFound},
		{"22021", "", interfaces.ErrAzureRegistryInvalid},
	} {
		err := mapAzureRegistryErr("op", fmt.Errorf("wrapped: %w", &pgconn.PgError{Code: tc.code, ConstraintName: tc.constraint, Message: "m"}))
		if !errors.Is(err, tc.want) {
			t.Errorf("%s on %s -> %v, want %v", tc.code, tc.constraint, err, tc.want)
		}
	}
	plain := errors.New("connection reset")
	if err := mapAzureRegistryErr("list resources", plain); !errors.Is(err, plain) || !strings.Contains(err.Error(), "list resources") {
		t.Errorf("a plain error = %v, want it wrapped with the operation", err)
	}
	if err := mapAzureRegistryErr("op", interfaces.ErrAzureTooManyModels); !errors.Is(err, interfaces.ErrAzureTooManyModels) || err.Error() != interfaces.ErrAzureTooManyModels.Error() {
		t.Errorf("a typed error = %v, want it passed through as is", err)
	}
	if err := mapAzureRegistryErr("op", &pgconn.PgError{Code: "40001"}); errors.Is(err, interfaces.ErrAzureRegistryInvalid) {
		t.Errorf("an unrelated SQLSTATE mapped to %v", err)
	}
	if mapAzureRegistryErr("op", nil) != nil {
		t.Error("nil must map to nil")
	}
}

// TestAzureLockKey: the owner lock key is FNV-1a over the owner's lock name,
// distinct per owner and kind.
func TestAzureLockKey(t *testing.T) {
	u, o := azureLockKey(interfaces.AzureUserOwner("x")), azureLockKey(interfaces.AzureOrgOwner("x"))
	if u == o {
		t.Error("a user and an org with the same id share a lock")
	}
	if azureLockKey(interfaces.AzureUserOwner("x")) != u {
		t.Error("the lock key is not deterministic")
	}
	if interfaces.AzureUserOwner("x").LockName() != "azure-registry:user:x" || interfaces.AzureOrgOwner("x").LockName() != "azure-registry:org:x" {
		t.Errorf("lock names = %q, %q", interfaces.AzureUserOwner("x").LockName(), interfaces.AzureOrgOwner("x").LockName())
	}
}
