//go:build linux

package pi5

import (
	"context"
	"fmt"
	"sync"

	"github.com/pkg/errors"
	"go.viam.com/utils"

	"go.viam.com/rdk/components/board"
	"go.viam.com/rdk/components/board/pinwrappers"
)

type wrappedAnalogReader struct {
	mu         sync.RWMutex
	chipSelect string
	reader     *pinwrappers.AnalogSmoother
}

func newWrappedAnalogReader(ctx context.Context, chipSelect string, reader *pinwrappers.AnalogSmoother) *wrappedAnalogReader {
	var wrapped wrappedAnalogReader
	wrapped.reset(ctx, chipSelect, reader)
	fmt.Printf("called new wrap analg reader\n")
	return &wrapped
}

func (a *wrappedAnalogReader) Read(ctx context.Context, extra map[string]interface{}) (board.AnalogValue, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.reader == nil {
		return board.AnalogValue{}, errors.New("closed")
	}
	fmt.Printf("read \n")
	return a.reader.Read(ctx, extra)
}

func (a *wrappedAnalogReader) Close(ctx context.Context) error {
	fmt.Printf("close \n")
	return a.reader.Close(ctx)
}

func (a *wrappedAnalogReader) reset(ctx context.Context, chipSelect string, reader *pinwrappers.AnalogSmoother) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.reader != nil {
		utils.UncheckedError(a.reader.Close(ctx))
	}
	a.reader = reader
	a.chipSelect = chipSelect
	fmt.Printf("resett \n")
}

func (a *wrappedAnalogReader) Write(ctx context.Context, value int, extra map[string]interface{}) error {
	fmt.Printf("implementing write\n")
	return nil
	//return grpc.UnimplementedError
}
