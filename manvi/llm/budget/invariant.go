package budget

import (
	"errors"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"io"
	"os"
)

func copySnapshot(s Snapshot) Snapshot {
	s.Attempts = append([]Attempt(nil), s.Attempts...)
	for i := range s.Attempts {
		if s.Attempts[i].Usage != nil {
			u := *s.Attempts[i].Usage
			s.Attempts[i].Usage = &u
		}
	}
	return s
}
func usageCharge(p Prices, u llm.Usage) (int64, error) {
	if u.InputTokens <= 0 || u.OutputTokens < 0 || u.ReasoningTokens < 0 || u.CacheWriteTokens != 0 || u.CacheReadTokens < 0 || u.CacheReadTokens > u.InputTokens || u.InputTokens > p.MaxInputTokens || u.OutputTokens > p.MaxOutputTokens || u.ReasoningTokens > p.MaxOutputTokens-u.OutputTokens {
		return 0, errors.New("provider usage missing or outside reserved bounds; reservation retained")
	}
	return int64(u.InputTokens)*p.InputNanoUSD + int64(u.OutputTokens+u.ReasoningTokens)*p.OutputNanoUSD, nil
}
func validateSnapshot(s Snapshot) error {
	if len(s.Attempts) > 4096 {
		return errors.New("campaign attempt count exceeded")
	}
	reserve := int64(s.Prices.MaxInputTokens)*s.Prices.InputNanoUSD + int64(s.Prices.MaxOutputTokens)*s.Prices.OutputNanoUSD
	remaining := s.LimitNanoUSD
	for i, a := range s.Attempts {
		if a.ID != uint64(i+1) || a.ReservedNanoUSD < 0 || a.ChargedNanoUSD < 0 {
			return errors.New("invalid campaign attempt identity or amount")
		}
		switch a.State {
		case "reserved", "accepted", "rejected", "unknown":
			if a.ReservedNanoUSD != reserve || a.ChargedNanoUSD != 0 || a.Usage != nil {
				return errors.New("unresolved attempt must retain its full reservation")
			}
		case "not_sent":
			if a.ReservedNanoUSD != 0 || a.ChargedNanoUSD != 0 || a.Usage != nil {
				return errors.New("unsent attempt cannot carry a charge or usage")
			}
		case "settled":
			if a.Usage == nil || a.ReservedNanoUSD != 0 {
				return errors.New("settled attempt requires authoritative usage")
			}
			charge, err := usageCharge(s.Prices, *a.Usage)
			if err != nil || charge != a.ChargedNanoUSD {
				return errors.New("settled charge does not match usage")
			}
		default:
			return errors.New("invalid campaign attempt state")
		}
		if a.ReservedNanoUSD > remaining {
			return errors.New("campaign reservation exceeds remaining limit")
		}
		remaining -= a.ReservedNanoUSD
		if a.ChargedNanoUSD > remaining {
			return errors.New("campaign charge exceeds remaining limit")
		}
		remaining -= a.ChargedNanoUSD
	}
	return nil
}
func readLedger(path string) ([]byte, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Size() > 4<<20 {
		return nil, errors.New("campaign ledger must be a regular file within 4 MiB")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || !os.SameFile(before, info) {
		return nil, errors.New("campaign ledger changed while opening")
	}
	raw, err := io.ReadAll(io.LimitReader(f, (4<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > 4<<20 {
		return nil, errors.New("campaign ledger exceeds 4 MiB")
	}
	return raw, nil
}
