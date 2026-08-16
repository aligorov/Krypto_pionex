package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/aligorov/pionex-bot/backend/internal/auth"
	"github.com/aligorov/pionex-bot/backend/internal/database"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/term"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := connect(ctx)
	if err != nil {
		exitError(err)
	}
	defer db.Close()
	service := auth.NewService(db)

	switch os.Args[1] {
	case "create-user":
		createUser(ctx, service, os.Args[2:])
	case "reset-password":
		resetPassword(ctx, service, os.Args[2:])
	case "list-users":
		listUsers(ctx, service)
	default:
		usage()
		os.Exit(2)
	}
}

func createUser(ctx context.Context, service *auth.Service, args []string) {
	flags := flag.NewFlagSet("create-user", flag.ExitOnError)
	username := flags.String("username", "", "login name")
	displayName := flags.String("display-name", "", "display name")
	email := flags.String("email", "", "email address")
	role := flags.String("role", auth.RoleAdmin, "VIEWER, OPERATOR or ADMIN")
	mustChange := flags.Bool("must-change-password", false, "force password change after login")
	passwordStdin := flags.Bool("password-stdin", false, "read password from stdin")
	_ = flags.Parse(args)
	if *username == "" || *displayName == "" {
		exitError(errors.New("--username and --display-name are required"))
	}
	password, err := readPassword(*passwordStdin)
	if err != nil {
		exitError(err)
	}
	var emailValue *string
	if strings.TrimSpace(*email) != "" {
		emailValue = email
	}
	user, err := service.CreateUser(ctx, auth.CreateUserInput{
		Username: *username, DisplayName: *displayName, Email: emailValue,
		Password: password, Role: *role, MustChangePassword: *mustChange,
	})
	if err != nil {
		exitError(err)
	}
	fmt.Printf("created user %s (%s), id=%s\n", user.Username, user.Role, user.ID)
}

func resetPassword(ctx context.Context, service *auth.Service, args []string) {
	flags := flag.NewFlagSet("reset-password", flag.ExitOnError)
	userID := flags.String("user-id", "", "user UUID")
	mustChange := flags.Bool("must-change-password", true, "force password change after login")
	passwordStdin := flags.Bool("password-stdin", false, "read password from stdin")
	_ = flags.Parse(args)
	if *userID == "" {
		exitError(errors.New("--user-id is required"))
	}
	password, err := readPassword(*passwordStdin)
	if err != nil {
		exitError(err)
	}
	if err := service.ChangePassword(ctx, *userID, password, *mustChange); err != nil {
		exitError(err)
	}
	fmt.Println("password updated; all active sessions were revoked")
}

func listUsers(ctx context.Context, service *auth.Service) {
	users, err := service.ListUsers(ctx)
	if err != nil {
		exitError(err)
	}
	for _, user := range users {
		fmt.Printf("%s\t%s\t%s\tactive=%t\tid=%s\n",
			user.Username, user.DisplayName, user.Role, user.IsActive, user.ID,
		)
	}
}

func connect(ctx context.Context) (*pgxpool.Pool, error) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		// No credentials inside the binary (Zero-ENV policy, audit SEC-001).
		return nil, errors.New("DATABASE_URL is required")
	}
	db, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("create PostgreSQL pool: %w", err)
	}
	if err := db.Ping(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("connect PostgreSQL: %w", err)
	}
	directory := "./migrations"
	if _, err := os.Stat("/app/migrations"); err == nil {
		directory = "/app/migrations"
	}
	if err := database.Migrate(ctx, db, directory); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func readPassword(fromStdin bool) (string, error) {
	if fromStdin || !term.IsTerminal(int(os.Stdin.Fd())) {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && strings.TrimSpace(line) == "" {
			return "", fmt.Errorf("read password: %w", err)
		}
		return strings.TrimSpace(line), nil
	}
	fmt.Fprint(os.Stderr, "Password: ")
	first, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}
	fmt.Fprint(os.Stderr, "Confirm password: ")
	second, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", fmt.Errorf("confirm password: %w", err)
	}
	if string(first) != string(second) {
		return "", errors.New("passwords do not match")
	}
	return string(first), nil
}

func usage() {
	fmt.Fprintln(os.Stderr, "Usage: pionex-admin <create-user|reset-password|list-users> [flags]")
}

func exitError(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
