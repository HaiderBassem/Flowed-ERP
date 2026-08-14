package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/swibit/flowed/internal/adapter/postgres"
	"github.com/swibit/flowed/internal/app"
	"github.com/swibit/flowed/internal/domain/shared"
	"github.com/swibit/flowed/internal/platform/auth"
	"github.com/swibit/flowed/internal/platform/config"
	"github.com/swibit/flowed/internal/platform/logger"
	"github.com/swibit/flowed/internal/platform/pg"
)

// bootstrapActor is the identity the two account-creating commands run under.
//
// They are the only paths that may create an operator without an operator
// already existing, so there is no signed-in actor to attribute the change to.
// Naming the act "bootstrap" in the audit trail is more honest than attributing
// it to the account being created, which is what a self-referential actor would
// do.
func bootstrapActor() shared.Actor {
	return shared.Actor{
		Username: "bootstrap",
		Roles:    []shared.Role{shared.RoleAdmin},
		Scope:    shared.UniversityScope(),
	}
}

// userServiceFor wires the operator commands over a pool.
//
// The CLI drives the same application service the HTTP API does, so a user
// created from a terminal gets the same validation, the same password policy
// and the same audit entry as one created from the administration screen. The
// alternative — a second implementation in the command — is how the two drift
// until one of them is quietly wrong.
func userServiceFor(db *pg.DB, cfg *config.Config, log *slog.Logger) *app.UserService {
	deps := app.Deps{
		Tx:        postgres.NewTxManager(db),
		Users:     postgres.NewUserRepository(db),
		Reference: postgres.NewReferenceRepository(db),
		Audit:     postgres.NewAuditRepository(db),
		Clock:     shared.SystemClock{},
		Log:       log,
	}
	return app.NewUserService(
		deps,
		auth.NewHasher(cfg.Auth),
		postgres.NewSessionRepository(db),
		postgres.NewLoginAttemptRepository(db),
	)
}

// seed creates the first administrator so a fresh installation can be signed
// into.
//
// It refuses to run in production. A known starting password is exactly the
// kind of thing that survives into a live system for years, and a university
// finance system is not somewhere to find out. Production installations create
// their first account through a deliberate, one-off procedure with a password
// the operator chooses.
func seed() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.App.IsProduction() {
		return fmt.Errorf(
			"refusing to seed in production: create the first administrator deliberately, " +
				"with a password nobody else has seen")
	}

	log := logger.New(cfg.Log, cfg.App.Name+"-seed", version, cfg.App.Environment)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	db, err := pg.Connect(ctx, cfg.Database, log, nil)
	if err != nil {
		return err
	}
	defer db.Close()

	users := postgres.NewUserRepository(db)
	username := envOr("SEED_ADMIN_USERNAME", "admin")
	password := envOr("SEED_ADMIN_PASSWORD", "change-me-immediately")

	if existing, err := users.GetByUsername(ctx, username); err == nil && existing != nil {
		log.Info("administrator already exists, nothing to seed",
			slog.String("username", username))
		return nil
	}

	result, err := userServiceFor(db, cfg, log).CreateUser(ctx, bootstrapActor(), app.CreateUserInput{
		Username: username,
		FullName: "System Administrator",
		Roles:    []shared.Role{shared.RoleAdmin},
		Password: password,
	})
	if err != nil {
		return err
	}

	log.Info("administrator created", slog.String("username", username))
	fmt.Printf("\nAdministrator created.\n  username: %s\n  password: %s\n\n"+
		"This password must be changed at first sign-in; every other route refuses until it is.\n\n",
		username, password)
	_ = result
	return nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// createUser adds an operator with the roles named on the command line.
//
//	api create-user <username> "<full name>" <role>[,<role>...]
//
// The password is read from CREATE_USER_PASSWORD rather than an argument:
// arguments land in shell history and in the process list, where any other
// user on the machine can read them. Omitting it entirely is better still —
// the command then generates one and prints it once.
func createUser() error {
	args := os.Args[2:]
	if len(args) < 3 {
		return errors.New(`usage: api create-user <username> "<full name>" <role>[,<role>...]` +
			"\nthe password is read from CREATE_USER_PASSWORD, or generated when that is unset")
	}
	username, fullName, roleList := args[0], args[1], args[2]

	roles := make([]shared.Role, 0, 4)
	for _, name := range strings.Split(roleList, ",") {
		role := shared.Role(strings.TrimSpace(name))
		if !role.Valid() {
			return fmt.Errorf("%q is not a recognised role; valid roles are %v", name, shared.AllRoles)
		}
		roles = append(roles, role)
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := logger.New(cfg.Log, cfg.App.Name+"-admin", version, cfg.App.Environment)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	db, err := pg.Connect(ctx, cfg.Database, log, nil)
	if err != nil {
		return err
	}
	defer db.Close()

	result, err := userServiceFor(db, cfg, log).CreateUser(ctx, bootstrapActor(), app.CreateUserInput{
		Username: username,
		FullName: fullName,
		Roles:    roles,
		Password: os.Getenv("CREATE_USER_PASSWORD"),
	})
	if err != nil {
		return err
	}

	fmt.Printf("created %s (%s) with roles %v\n", username, result.User.ID, roles)
	if result.TemporaryPassword != "" {
		fmt.Printf("temporary password: %s\n", result.TemporaryPassword)
	}
	fmt.Println("the holder must change this password before any other route will answer them")
	return nil
}
