package commands

import (
	"context"
	"time"

	"github.com/urfave/cli/v2"

	"github.com/photoprism/photoprism/internal/photoprism/get"
	"github.com/photoprism/photoprism/pkg/clean"
)

// ThumbsCommand configures the command name, flags, and action.
var ThumbsCommand = &cli.Command{
	Name:      "thumbs",
	Usage:     "(Re-)generates thumbnails based on the current configuration",
	ArgsUsage: "[subfolder]",
	Flags: []cli.Flag{
		&cli.BoolFlag{
			Name:    "force",
			Aliases: []string{"f"},
			Usage:   "replace existing thumbnail images",
		},
		&cli.BoolFlag{
			Name:    "originals",
			Aliases: []string{"o"},
			Usage:   "scan originals only, skip sidecar folder",
		},
		&cli.BoolFlag{
			Name:    "pending",
			Aliases: []string{"p"},
			Usage:   "generate only the sizes recorded as pending or failed, without scanning folders",
		},
	},
	Action: thumbsAction,
}

// thumbsAction regenerates thumbnails based on the current configuration.
func thumbsAction(ctx *cli.Context) error {
	start := time.Now()

	conf, err := InitConfig(ctx)

	_, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err != nil {
		return err
	}

	defer conf.Shutdown()

	dir, err := sanitizeSubfolderArg(ctx.Args().First())

	if err != nil {
		return err
	}

	force := ctx.Bool("force")
	originals := ctx.Bool("originals")
	pending := ctx.Bool("pending")

	w := get.Thumbs()

	// Generate only the sizes recorded as pending or failed, without a folder scan.
	if pending {
		log.Infof("generating pending thumbnails only")

		result, pendingErr := w.StartPending()
		if pendingErr != nil {
			return pendingErr
		}

		log.Infof("pending thumbnails processed in %s (%d processed, %d repaired, %d remaining, %d pruned)",
			time.Since(start), result.Processed, result.Repaired, result.Remaining, result.Pruned)

		return nil
	}

	var action, done string
	if force {
		action = "replacing"
		done = "replaced"
	} else {
		action = "generating"
		done = "generated"
	}

	// Display info.
	if dir == "" {
		if originals {
			log.Infof("%s thumbnails for originals only", action)
		} else {
			log.Infof("%s thumbnails for originals and sidecar files", action)
		}
	} else {
		if originals {
			log.Infof("%s thumbnails for originals in %s", action, clean.LogQuote(dir))
		} else {
			log.Infof("%s thumbnails for originals and sidecar files in %s", action, clean.LogQuote(dir))
		}
	}

	if err = w.Start(dir, force, originals); err != nil {
		return err
	}

	log.Infof("thumbnails %s in %s", done, time.Since(start))

	return nil
}
