package main

import (
	"os"
	"os/exec"

	"github.com/codegangsta/cli"
	"go.uber.org/zap"
)

var (
	gopath     string
	alphasFile string
	logger     *zap.Logger
)

func init() {
	logger = newLogger()
}

func main() {
	defer logger.Sync()

	app := cli.NewApp()
	app.Name = "zordon"
	app.Usage = "Defends development environment from Rita, and her endless waves of containers!"
	app.Authors = []cli.Author{
		{
			Name:  "Piotr Kowalczuk",
			Email: "p.kowalczuk.priv@gmail.com",
		},
	}
	app.Version = "0.1.0"
	app.EnableBashCompletion = true
	app.Flags = []cli.Flag{
		cli.StringFlag{
			Name:        "gopath",
			EnvVar:      "GOPATH",
			Destination: &gopath,
		},
		cli.StringFlag{
			Name:        "alphasfile",
			Value:       "Alphasfile",
			Destination: &alphasFile,
		},
	}
	app.Commands = []cli.Command{
		{
			Name:        "morphintime",
			Aliases:     []string{"mt"},
			Description: "Rangers, you must act swiftly, the development environment is in grave danger!",
			Action:      morphin,
			Before:      summon,
			Flags: []cli.Flag{
				cli.BoolFlag{
					Name: "install",
				},
			},
		},
		{
			Name:        "recruit",
			Aliases:     []string{"r"},
			Description: "Alpha, Rita's escaped! Recruit a team of services with attitude!",
			Action:      recruit,
			Before:      summon,
		},
		{
			Name:        "powerup",
			Aliases:     []string{"pu"},
			Description: "We need Thunderzord power now!",
			Action:      powerup,
			Before:      summon,
		},
		{
			Name:        "list",
			Aliases:     []string{"ls"},
			Description: "List services managed by any running zordon instance.",
			Action:      list,
		},
	}

	app.Run(os.Args)
}

func summon(ctx *cli.Context) error {
	return nil
}

func serviceLogger(l *zap.Logger, s *Service) *zap.Logger {
	return l.With(zap.String(keySubsystem, s.Name), zap.String(keyColor, s.Color))
}

func isGitModifiedLocaly(s *Service) (bool, error) {
	src, err := resolveSource(s)
	if err != nil {
		return false, err
	}
	check := exec.Command("git", "-C", src.repoDir, "diff", "--exit-code")
	if err := run(check, s, zap.NewNop()); err != nil {
		if check.ProcessState.Exited() {
			return true, nil
		}
		return false, err
	}

	return false, nil
}

func updateRepository(s *Service) (err error) {
	src, err := resolveSource(s)
	if err != nil {
		return err
	}
	branch := s.Branch()
	fetch := exec.Command("git", "-C", src.repoDir, "fetch", "-q", "origin", branch)
	if err = run(fetch, s, logger); err != nil {
		return
	}
	checkout := exec.Command("git", "-C", src.repoDir, "checkout", "-q", branch)
	if err = run(checkout, s, logger); err != nil {
		return
	}
	pull := exec.Command("git", "-C", src.repoDir, "pull", "-q", "origin", branch)
	if err = run(pull, s, logger); err != nil {
		return
	}

	return
}
