package chat

import (
	"context"
	"errors"
	"github.com/openimsdk/chat/pkg/common/imapi"
	"github.com/openimsdk/chat/pkg/common/messageextras"
	"github.com/openimsdk/protocol/msg"
	"github.com/openimsdk/protocol/sdkws"
	"sort"
	"time"
)

// Sequence hints only locate messages. Every response rechecks actor visibility
// through OpenIM's pull projection (min/max, per-user deletions and revocation).
func (o *Api) visibleExtraRecords(ctx context.Context, actor, conversationID string, minSeq, maxSeq int64, records map[string]*messageextras.Record) (map[string]*messageextras.Record, error) {
	result := map[string]*messageextras.Record{}
	if len(records) == 0 {
		return result, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	progress, err := imapi.NewApiCaller[msg.GetConversationsHasReadAndMaxSeqReq, msg.GetConversationsHasReadAndMaxSeqResp]("/msg/get_conversations_has_read_and_max_seq").Call(ctx, o.extrasIMURL, &msg.GetConversationsHasReadAndMaxSeqReq{UserID: actor, ConversationIDs: []string{conversationID}})
	if err != nil {
		return nil, err
	}
	if progress == nil || progress.Seqs[conversationID] == nil {
		return nil, errors.New("missing message visibility bounds")
	}
	upper := progress.Seqs[conversationID].MaxSeq
	if maxSeq > 0 {
		upper = min(upper, maxSeq)
	}
	lower := max(int64(1), minSeq)
	if upper < lower {
		return result, nil
	}
	known := map[int64][]string{}
	unknown := map[string]bool{}
	for id, record := range records {
		if record == nil {
			continue
		}
		seq := record.Seq
		if seq == 0 {
			seq = extraSequenceHints.get(o.extrasIMURL, conversationID, id)
		}
		if seq == 0 {
			unknown[id] = true
		} else if seq >= lower && seq <= upper {
			known[seq] = append(known[seq], id)
		}
	}
	pull := func(begin, end int64) (*sdkws.PullMsgs, error) {
		response, err := imapi.NewApiCaller[sdkws.PullMessageBySeqsReq, sdkws.PullMessageBySeqsResp]("/msg/pull_msg_by_seq").Call(ctx, o.extrasIMURL, &sdkws.PullMessageBySeqsReq{UserID: actor, Order: sdkws.PullOrder_PullOrderDesc, SeqRanges: []*sdkws.SeqRange{{ConversationID: conversationID, Begin: begin, End: end, Num: end - begin + 1}}})
		if err != nil {
			return nil, err
		}
		if response == nil || response.Msgs[conversationID] == nil {
			return nil, errors.New("message visibility unavailable")
		}
		return response.Msgs[conversationID], nil
	}
	collect := func(row *sdkws.MsgData) {
		if row == nil || row.Seq < lower || row.Seq > upper {
			return
		}
		record := records[row.ClientMsgID]
		if record == nil {
			return
		}
		expected := record.Seq
		if expected == 0 {
			expected = extraSequenceHints.get(o.extrasIMURL, conversationID, row.ClientMsgID)
		}
		if expected != 0 && row.Seq != expected {
			return
		}
		// An ID/sequence match locates a row, but never authorizes its content.
		extraSequenceHints.put(o.extrasIMURL, conversationID, row.ClientMsgID, row.Seq)
		delete(unknown, row.ClientMsgID)
		if row.Status >= 4 || row.ContentType >= 1000 {
			return
		}
		copy := *record
		copy.Seq = row.Seq
		result[row.ClientMsgID] = &copy
	}

	seqs := make([]int64, 0, len(known))
	for seq := range known {
		seqs = append(seqs, seq)
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	for i := 0; i < len(seqs); {
		begin, end := seqs[i], seqs[i]
		i++
		for i < len(seqs) && seqs[i]-begin < 500 {
			end = seqs[i]
			i++
		}
		page, err := pull(begin, end)
		if err != nil {
			return nil, err
		}
		for _, row := range page.Msgs {
			if row != nil && row.Seq >= begin && row.Seq <= end {
				collect(row)
			}
		}
	}
	// Old records do not have seq. Scan only the current user's visible history,
	// bounded by request deadline and 500 sequences per call; never admin search.
	keys := map[string]legacyScanKey{}
	cursors := map[string]int64{}
	end := int64(0)
	for id := range unknown {
		key := legacyScanKey{o.extrasIMURL, actor, conversationID, id, lower}
		keys[id] = key
		next := extraLegacyScans.get(key, upper)
		if next < lower {
			delete(unknown, id)
		} else {
			cursors[id] = next
			if end == 0 || next < end {
				end = next
			}
		}
	}
	for pages := 0; len(unknown) > 0 && end >= lower; pages++ {
		if pages >= 8 {
			return nil, errors.New("旧消息索引正在补齐，请稍后重试")
		}
		begin := max(lower, end-499)
		page, err := pull(begin, end)
		if err != nil {
			return nil, err
		}
		if len(page.Msgs) == 0 && !page.IsEnd {
			return nil, errors.New("incomplete legacy visibility page")
		}
		sort.Slice(page.Msgs, func(i, j int) bool { return page.Msgs[i].GetSeq() > page.Msgs[j].GetSeq() })
		expected := end
		for _, row := range page.Msgs {
			if row == nil || row.Seq != expected || row.Seq < begin {
				return nil, errors.New("incomplete legacy visibility sequence")
			}
			collect(row)
			expected--
		}
		if expected >= begin && !page.IsEnd {
			return nil, errors.New("truncated legacy visibility page")
		}
		next := begin - 1
		if page.IsEnd {
			next = 0
		}
		for id := range unknown {
			if cursors[id] != end {
				continue
			}
			extraLegacyScans.put(keys[id], next)
			cursors[id] = next
			if next < lower {
				delete(unknown, id)
			}
		}
		end = 0
		for id := range unknown {
			if end == 0 || cursors[id] < end {
				end = cursors[id]
			}
		}
	}

	for id, record := range result {
		extraSequenceHints.put(o.extrasIMURL, conversationID, id, record.Seq)
	}
	return result, nil
}
