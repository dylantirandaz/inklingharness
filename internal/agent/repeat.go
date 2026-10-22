package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"github.com/dylantirandaz/inklingharness/internal/anthropic"
	"github.com/dylantirandaz/inklingharness/internal/tools"
)

// repeatedReads replaces the result of a read that repeats an earlier read
// with a short note, in the lean profile. On SWE-bench, 169 of 8,815 results
// of Think were a read_file call with the same input and the same result as
// an earlier call of the same run; they were 4.5% of all tool-result
// characters, and each later turn reads them again from the prompt cache.
//
// The note replaces only a result that is equal to an earlier result that is
// still in the context: the model loses no information. A changed file gives
// a different result, so it comes back in full. After a note, the next equal
// call gets the full result again, because a model that asks again may need
// the text near its newest message. Compaction removes earlier results, so it
// must call forget.
type repeatedReads struct {
	earlier map[string]earlierRead
}

type earlierRead struct {
	id   string
	hash [sha256.Size]byte
}

func newRepeatedReads() *repeatedReads {
	return &repeatedReads{earlier: map[string]earlierRead{}}
}

// forget drops every earlier result, because they are no longer in the
// context. A nil receiver does nothing.
func (r *repeatedReads) forget() {
	if r != nil {
		clear(r.earlier)
	}
}

// result returns the content that the model gets for call. A nil receiver
// returns result unchanged.
func (r *repeatedReads) result(call anthropic.ToolUseBlock, result tools.Result) tools.Result {
	if r == nil || result.IsError {
		return result
	}
	switch call.Name {
	case "read_file", "read_files":
	default:
		return result
	}
	var input bytes.Buffer
	if err := json.Compact(&input, call.Input); err != nil {
		return result
	}
	key := call.Name + "\x00" + input.String()
	hash := sha256.Sum256([]byte(result.Content))
	if earlier, found := r.earlier[key]; found && earlier.hash == hash {
		note := fmt.Sprintf("[This result is the same as the result of the earlier %s call %s, which had the same input. The content did not change. Use that result.]", call.Name, earlier.id)
		if len(note) < len(result.Content) {
			delete(r.earlier, key)
			return tools.Result{Content: note}
		}
	}
	r.earlier[key] = earlierRead{id: call.ID, hash: hash}
	return result
}
