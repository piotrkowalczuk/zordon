package main

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/codegangsta/cli"
	"go.uber.org/zap"
	"k8s.io/utils/strings/slices"
)

func powerup(ctx *cli.Context) error {
	af, err := openAlphasfile(alphasFile)
	if err != nil {
		logger.Fatal(err.Error())
	}

	all := af.All()
	if len(all) == 0 {
		return nil
	}

	serviceLogger(logger, all[0]).Warn("We need Thunderzord power now!")

	for _, s := range all {
		rl := serviceLogger(logger, s)
		modified, err := isGitModifiedLocaly(s)
		if err != nil {
			rl.Fatal("Ayiyiyiyi!", zap.Error(err))
		}
		if modified {
			logger.Warn(fmt.Sprintf("Alpha and I will have to analyze your powers, %s, to see if they can be restored to you permanently.", s.Name), zap.String(keySubsystem, "zordon"))
			continue
		}

		if slices.Contains([]string{"", "master", "main"}, s.Branch()) {
			update := exec.Command("go", "get", "-u", "-t", s.Source())
			if err := run(update, s, rl); err != nil {
				rl.Fatal("Ayiyiyiyi!", zap.Error(err))
			}
		} else {
			if err := updateRepository(s); err != nil {
				rl.Fatal("Ayiyiyiyi!", zap.Error(err))
			}
		}
		rl.Info(fmt.Sprintf("%s Thunderzord Power!", strings.ToTitle(s.Name)))
	}

	logger.Info("ThunderZords shall be yours, powerful and agile. When joined together, all shall form the Thunder MegaZord.", zap.String(keySubsystem, "zordon"))
	return nil
}
