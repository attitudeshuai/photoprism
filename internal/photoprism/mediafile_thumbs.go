package photoprism

import (
	"bytes"
	"fmt"
	"image"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/dustin/go-humanize/english"

	"github.com/photoprism/photoprism/internal/thumb"
	"github.com/photoprism/photoprism/pkg/capture"
	"github.com/photoprism/photoprism/pkg/clean"
	"github.com/photoprism/photoprism/pkg/fs"
	"github.com/photoprism/photoprism/pkg/fs/disk"
	"github.com/photoprism/photoprism/pkg/log/status"
	"github.com/photoprism/photoprism/pkg/proc"
)

// noSpaceError reports whether a render error means the target filesystem is full.
// It is a package variable so storage-abort handling can be exercised without a full disk.
var noSpaceError = disk.IsNoSpace

// Bounds returns the media dimensions as image.Rectangle.
func (m *MediaFile) Bounds() image.Rectangle {
	return image.Rectangle{Min: image.Point{}, Max: image.Point{X: m.Width(), Y: m.Height()}}
}

// Thumbnail returns a thumbnail filename.
func (m *MediaFile) Thumbnail(path string, sizeName thumb.Name) (filename string, err error) {
	size, ok := thumb.Sizes[sizeName]

	if !ok {
		log.Errorf("media: invalid type %s", sizeName)
		return "", fmt.Errorf("media: invalid type %s", sizeName)
	}

	// Choose the smallest fitting size if the original image is smaller.
	if size.Fit && m.Bounds().In(size.Bounds()) {
		size = thumb.FitBounds(m.Bounds())
		log.Tracef("media: smallest fitting size for %s is %s (width %d, height %d)", clean.Log(m.RootRelName()), size.Name, size.Width, size.Height)
	}

	thumbName, err := size.FromFile(m.FileName(), m.Hash(), path, m.Orientation())

	if err != nil {
		err = fmt.Errorf("media: failed to create thumbnail for %s (%w)", clean.Log(m.BaseName()), err)
		log.Debug(err)
		return "", err
	}

	return thumbName, nil
}

// Resample returns a resampled image of the file.
func (m *MediaFile) Resample(path string, sizeName thumb.Name) (img image.Image, err error) {
	thumbName, err := m.Thumbnail(path, sizeName)

	if err != nil {
		return nil, err
	}

	img, _, err = fs.DecodeImageFile(thumbName)

	return img, err
}

// SkipThumbnailSize tests if the thumbnail size can be skipped,
// e.g. because it is larger than the original.
func (m *MediaFile) SkipThumbnailSize(size thumb.Size) bool {
	return thumb.Skip(size, m.Bounds())
}

// configBounds returns the media dimensions from the image header, without the orientation
// correction that Bounds reads from the file metadata. An undecodable header yields the zero
// rectangle, which thumb.Skip treats as a square source smaller than every size.
func (m *MediaFile) configBounds() image.Rectangle {
	cfg, err := m.DecodeConfig()

	if err != nil {
		return image.Rectangle{}
	}

	return image.Rectangle{Min: image.Point{}, Max: image.Point{X: cfg.Width, Y: cfg.Height}}
}

// GenerateThumbnails generates thumbnails in the specified storage path,
// existing images are only replaced if the force flag is set to true.
func (m *MediaFile) GenerateThumbnails(thumbPath string, force bool) (err error) {
	return m.generateThumbnails(thumbPath, force, m.Bounds, newDbThumbResultSink(m))
}

// GenerateAvatarThumbnails generates thumbnails for a user-provided avatar image, sizing them
// from the image header so that the file metadata is not parsed. libvips applies the embedded
// orientation itself; the fallback imaging library still reads it where it must rotate.
func (m *MediaFile) GenerateAvatarThumbnails(thumbPath string, force bool) (err error) {
	return m.generateThumbnails(thumbPath, force, m.configBounds, nopThumbResultSink{})
}

// generateThumbnails generates the cached thumbnail sizes, skipping those the source bounds
// make redundant. Per-size failures are recorded but do not abort the other sizes; only a
// full disk or an unreadable source abort the file. The bounds are resolved only once a size
// actually needs generating, because resolving them reads the file.
func (m *MediaFile) generateThumbnails(thumbPath string, force bool, srcBounds func() image.Rectangle, sink thumbResultSink) (err error) {
	if !m.IsPreviewImage() {
		// Skip.
		return
	}

	count := 0
	start := time.Now()

	// Surface a disk-full write failure as the shared insufficient-storage sentinel,
	// so the indexer and thumbs worker can abort the run instead of failing every file.
	defer func() {
		err = disk.AsInsufficientStorage(err)
	}()

	defer func() {
		switch count {
		case 0:
			log.Debug(capture.Time(start, fmt.Sprintf("media: generated no new thumbnails for %s", clean.Log(m.RootRelName()))))
		default:
			log.Info(capture.Time(start, fmt.Sprintf("media: generated %s for %s", english.Plural(count, "thumbnail", "thumbnails"), clean.Log(m.RootRelName()))))
		}
	}()

	hash := m.Hash()

	// Sizes within the configured cache limit are part of this run; register them as pending
	// before the first write so an aborted run leaves an actionable to-do list behind.
	eligible := eligibleThumbNames()

	sink.Start(eligible)

	var original image.Image
	var srcImage image.Image
	var srcBuffer []byte
	var srcName thumb.Name

	// failRemaining records a fatal source error for every eligible size, so the retry list
	// shows the file and its missing sizes.
	failRemaining := func(cause error) {
		for _, name := range eligible {
			sink.Failed(name, cause)
		}
	}

	for _, name := range eligible {
		var size thumb.Size
		var fileName string

		size = thumb.Sizes[name]

		if fileName, err = size.FileName(hash, thumbPath); err != nil {
			log.Errorf("media: failed to create %s thumbnail for %s (%s)", name, clean.Log(m.RootRelName()), clean.Error(err))
			sink.Failed(name, err)
			return err
		} else if !force && fs.FileExists(fileName) {
			// The cached file already exists; record the size as present without rendering it.
			sink.Succeeded(name)
			continue
		}

		var sizeErr error

		// Use libvips to generate thumbnails?
		if thumb.Library == thumb.LibVips {
			// Only create a thumbnail if its size does not exceed the size of the original image.
			if thumb.Skip(size, srcBounds()) {
				log.Tracef("media: skipped %s thumbnail for %s (larger than original)", name, clean.Log(m.RootRelName()))
				sink.Skipped(name, "larger than original")
				continue
			} else if size.Source != "" {
				// Original image filename.
				srcFile := m.FileName()

				// If possible, use existing thumbnail file to create smaller sizes.
				if thumbFile, srcErr := thumb.Sizes[size.Source].FileName(hash, thumbPath); srcErr == nil && fs.FileExistsNotEmpty(thumbFile) {
					srcFile = thumbFile
				}

				// Generate thumbnail with libvips.
				if size.Source == srcName && srcName != "" && srcBuffer != nil {
					_, _, sizeErr = thumb.Vips(srcFile, srcBuffer, hash, thumbPath, size.Width, size.Height, size.Options...)
				} else {
					_, _, sizeErr = thumb.Vips(srcFile, nil, hash, thumbPath, size.Width, size.Height, size.Options...)
				}

				// Log error, if any.
				if sizeErr != nil {
					log.Debugf("vips: %s in %s (generate thumbnail from %s)", sizeErr, clean.Log(m.RootRelName()), size.Source)
				}
			} else {
				var newBuffer []byte

				if _, newBuffer, sizeErr = thumb.Vips(m.FileName(), nil, hash, thumbPath, size.Width, size.Height, size.Options...); sizeErr != nil || newBuffer == nil {
					log.Debugf("vips: failed to generate thumbnail from %s", clean.Log(m.RootRelName()))
					// Clear thumbnail image cache; an empty buffer keeps the previous behavior
					// of continuing with the next size while the reusable buffer stays empty.
					srcBuffer = nil
					srcName = ""
				} else {
					srcBuffer = newBuffer
					// Remember cached thumbnail size.
					srcName = name
				}
			}
		} else {
			// Open original if needed.
			if original == nil {
				img, imgErr := thumb.Open(m.FileName(), m.Orientation())

				// Failed to open the JPEG file?
				if imgErr != nil {
					msg := imgErr.Error()

					// Non-repairable file error?
					if !strings.Contains(msg, fs.EOF.Error()) &&
						!strings.HasPrefix(msg, "invalid JPEG") {
						log.Debugf("media: %s in %s", msg, clean.Log(m.RootRelName()))
						failRemaining(imgErr)
						return imgErr
					}

					// Try to repair the file by creating a properly encoded copy with ImageMagick.
					if fixed, fixErr := NewConvert(conf).FixJpeg(m, false); fixErr != nil {
						failRemaining(fixErr)
						return fixErr
					} else if fixedImg, openErr := thumb.Open(fixed.FileName(), m.Orientation()); openErr != nil {
						failRemaining(openErr)
						return openErr
					} else {
						img = fixedImg
					}
				}

				original = img

				log.Debugf("media: opened %s [%s]", clean.Log(m.RootRelName()), thumb.MemSize(original).String())
			}

			// Thumb size too large
			// for the original image?
			if size.Skip(original) {
				log.Tracef("media: skipped %s thumbnail for %s (larger than original)", name, clean.Log(m.RootRelName()))
				sink.Skipped(name, "larger than original")
				continue
			}

			// Reuse existing thumbnails to improve rendering performance.
			if size.Source != "" {
				if size.Source == srcName && srcImage != nil {
					_, sizeErr = size.Create(srcImage, fileName)
				} else {
					_, sizeErr = size.Create(original, fileName)
				}
			} else {
				srcImage, sizeErr = size.Create(original, fileName)

				if sizeErr == nil {
					srcName = name
				}
			}
		}

		// A full disk aborts the run; leave the sizes that were never attempted pending.
		if noSpaceError(sizeErr) {
			log.Errorf("media: failed to create %s thumbnail for %s (%s)", name, clean.Log(m.RootRelName()), clean.Error(sizeErr))
			sink.Failed(name, sizeErr)
			err = status.ErrInsufficientStorage
			return
		}

		// Other per-size failures are recorded, and the remaining sizes keep generating.
		if sizeErr != nil {
			log.Errorf("media: failed to create %s thumbnail for %s (%s)", name, clean.Log(m.RootRelName()), clean.Error(sizeErr))
			sink.Failed(name, sizeErr)
			continue
		}

		sink.Succeeded(name)
		count++
	}

	return nil
}

// ChangeOrientation changes the file orientation.
func (m *MediaFile) ChangeOrientation(val int) (err error) {
	if !m.IsPreviewImage() {
		// Skip.
		return fmt.Errorf("orientation can currently only be changed for jpeg and png files")
	}

	cnf := Config()
	// #nosec G204 -- arguments are built from validated config and file paths.
	cmd := exec.Command(cnf.ExifToolBin(), "-overwrite_original", "-n", "-Orientation="+strconv.Itoa(val), m.FileName())

	// Fetch command output.
	var out bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	cmd.Env = append(cmd.Env, []string{
		fmt.Sprintf("HOME=%s", cnf.CmdCachePath()),
	}...)

	// Log exact command for debugging in trace mode.
	log.Trace(clean.Cmd(cmd))

	// Run exiftool command.
	if err = proc.Run(cmd, cnf.ConvertTimeout()); err != nil {
		if s := stderr.String(); s != "" {
			err = fmt.Errorf("%w: %s", err, s)
		}

		LogConvertError(err, cmd, clean.Log(m.BaseName()))

		return err
	}

	return nil
}
