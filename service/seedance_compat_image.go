package service

import (
	"context"
	"errors"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/webp"
	"golang.org/x/sync/semaphore"
)

var (
	seedanceImageDecodeSlots  = make(chan struct{}, 2)
	seedanceImageDecodeMemory = semaphore.NewWeighted(320 << 20)
)

func acquireSeedanceImageDecode(ctx context.Context, config image.Config) (func(), error) {
	select {
	case seedanceImageDecodeSlots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	memory := int64(config.Width)*int64(config.Height)*8 + 8<<20
	if err := seedanceImageDecodeMemory.Acquire(ctx, memory); err != nil {
		<-seedanceImageDecodeSlots
		return nil, err
	}
	return func() {
		seedanceImageDecodeMemory.Release(memory)
		<-seedanceImageDecodeSlots
	}, nil
}

// ValidateSeedanceCompatImage 限制图片解码资源，并在结束后重置上传流。
func ValidateSeedanceCompatImage(ctx context.Context, reader io.ReadSeeker) (resultErr error) {
	select {
	case seedanceImageDecodeSlots <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-seedanceImageDecodeSlots }()
	defer func() {
		if _, err := reader.Seek(0, io.SeekStart); err != nil {
			resultErr = errors.Join(resultErr, err)
		}
	}()
	if _, err := reader.Seek(0, io.SeekStart); err != nil {
		return err
	}
	config, _, err := image.DecodeConfig(reader)
	if err != nil || config.Width <= 0 || config.Height <= 0 || config.Width > seedanceAssetImageMaxDimension || config.Height > seedanceAssetImageMaxDimension {
		return errors.New("invalid image or image dimensions")
	}
	memory := int64(config.Width)*int64(config.Height)*8 + 8<<20
	if err := seedanceImageDecodeMemory.Acquire(ctx, memory); err != nil {
		return err
	}
	defer seedanceImageDecodeMemory.Release(memory)
	if _, err := reader.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, _, err := image.Decode(reader); err != nil {
		return errors.New("invalid image")
	}
	return ctx.Err()
}
