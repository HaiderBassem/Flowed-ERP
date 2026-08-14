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
	"github.com/swibit/flowed/internal/domain/shared"
	"github.com/swibit/flowed/internal/platform/auth"
	"github.com/swibit/flowed/internal/platform/config"
	"github.com/swibit/flowed/internal/platform/logger"
	"github.com/swibit/flowed/internal/platform/pg"
	"github.com/swibit/flowed/internal/port"
)

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
	txManager := postgres.NewTxManager(db)
	hasher := auth.NewHasher(cfg.Auth)

	username := envOr("SEED_ADMIN_USERNAME", "admin")
	password := envOr("SEED_ADMIN_PASSWORD", "change-me-immediately")

	if err := auth.ValidatePassword(password); err != nil {
		return fmt.Errorf("the seed password is not acceptable: %w", err)
	}

	if existing, err := users.GetByUsername(ctx, username); err == nil && existing != nil {
		log.Info("administrator already exists, nothing to seed",
			slog.String("username", username))
		return nil
	}

	hash, err := hasher.Hash(password)
	if err != nil {
		return err
	}

	admin := &port.User{
		ID:           shared.NewID(),
		Username:     username,
		FullName:     "System Administrator",
		PasswordHash: hash,
		IsActive:     true,
		Roles:        []shared.Role{shared.RoleAdmin},
	}

	// The user row and its roles land together: an administrator created
	// without their role is an account nobody can use and nobody can grant
	// anything to.
	err = txManager.Write(ctx, func(ctx context.Context) error {
		if err := users.Create(ctx, admin); err != nil {
			return err
		}
		return users.SetRoles(ctx, admin.ID, admin.Roles, admin.ID)
	})
	if err != nil {
		return err
	}

	log.Info("administrator created", slog.String("username", username))
	fmt.Printf("\nAdministrator created.\n  username: %s\n  password: %s\n\n"+
		"Change this password before anyone else uses the system.\n\n",
		username, password)
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
// user on the machine can read them.
func createUser() error {
	args := os.Args[2:]
	if len(args) < 3 {
		return errors.New(`usage: api create-user <username> "<full name>" <role>[,<role>...]` +
			"\nthe password is read from CREATE_USER_PASSWORD")
	}
	username, fullName, roleList := args[0], args[1], args[2]

	password := os.Getenv("CREATE_USER_PASSWORD")
	if password == "" {
		return errors.New("set CREATE_USER_PASSWORD; passing a password as an argument " +
			"leaves it in shell history and in the process list")
	}
	if err := auth.ValidatePassword(password); err != nil {
		return err
	}

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

	users := postgres.NewUserRepository(db)
	txManager := postgres.NewTxManager(db)

	hash, err := auth.NewHasher(cfg.Auth).Hash(password)
	if err != nil {
		return err
	}

	user := &port.User{
		ID:           shared.NewID(),
		Username:     username,
		FullName:     fullName,
		PasswordHash: hash,
		IsActive:     true,
		Roles:        roles,
	}

	err = txManager.Write(ctx, func(ctx context.Context) error {
		if err := users.Create(ctx, user); err != nil {
			return err
		}
		return users.SetRoles(ctx, user.ID, roles, user.ID)
	})
	if err != nil {
		return err
	}

	fmt.Printf("created %s (%s) with roles %v\n", username, user.ID, roles)
	return nil
}
