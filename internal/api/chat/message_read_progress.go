package chat

import (
	"context"
	"github.com/openimsdk/chat/pkg/common/imapi"
	"github.com/openimsdk/protocol/msg"
	"sync"
)

// Upstream exposes one user's progress per request. Bound aggregate concurrency
// across requests instead of multiplying an eight-worker pool per open dialog.
func (o *Api) loadReaderProgress(ctx context.Context, conversationID string, seq int64, members []messageReader) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	var once sync.Once
	var firstErr error
	fail := func(err error) { once.Do(func() { firstErr = err; cancel() }) }
	jobs := make(chan int)
	for n := 0; n < min(8, len(members)); n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				if err := acquireProgressSlot(ctx, readerProgressSlots); err != nil {
					fail(err)
					return
				}
				progress, err := imapi.NewApiCaller[msg.GetConversationsHasReadAndMaxSeqReq, msg.GetConversationsHasReadAndMaxSeqResp]("/msg/get_conversations_has_read_and_max_seq").Call(ctx, o.extrasIMURL, &msg.GetConversationsHasReadAndMaxSeqReq{UserID: members[i].UserID, ConversationIDs: []string{conversationID}})
				<-readerProgressSlots
				if err != nil {
					fail(err)
					return
				}
				if progress == nil || progress.Seqs[conversationID] == nil {
					fail(errReadProgressIncomplete)
					return
				}
				members[i].Read = progress.Seqs[conversationID].HasReadSeq >= seq
			}
		}()
	}
feed:
	for i := range members {
		select {
		case jobs <- i:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}
