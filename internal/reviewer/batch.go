package reviewer

import (
	"fmt"
	"strings"

	"github.com/inful/mreview/internal/gitlab"
	"github.com/inful/mreview/internal/llm"
)

// batchChunks groups consecutive chunks into batches. When
// maxBytes > 0, small chunks are greedily packed together as long
// as the sum of their Size stays under maxBytes — this lets
// operators with large-context models collapse N file-level
// reviews into fewer LLM calls. When maxBytes <= 0, every chunk
// gets its own batch (one call per chunk) — the historical
// behaviour, preserved as the default for backwards compatibility.
//
// A single chunk larger than maxBytes is placed alone in its own
// batch so it still gets reviewed; the LLM is the one that fails
// if it can't fit, and that failure surfaces the same way as any
// other chunk-level error.
func batchChunks(chunks []llm.Chunk, maxBytes int) [][]llm.Chunk {
	if len(chunks) == 0 {
		return nil
	}
	if maxBytes <= 0 {
		// No packing: one batch per chunk (original behaviour).
		out := make([][]llm.Chunk, len(chunks))
		for i, c := range chunks {
			out[i] = []llm.Chunk{c}
		}
		return out
	}
	// Greedy pack: walk chunks in order, accumulate into the
	// current batch until adding the next chunk would exceed
	// maxBytes. Then flush and start a new batch. Ordering is
	// preserved (chunks within a batch are in the same order as
	// the input slice) so the LLM sees a coherent diff narrative.
	var batches [][]llm.Chunk
	var current []llm.Chunk
	currentBytes := 0
	for _, c := range chunks {
		size := chunkBytes(c)
		if len(current) > 0 && currentBytes+size > maxBytes {
			batches = append(batches, current)
			current = nil
			currentBytes = 0
		}
		current = append(current, c)
		currentBytes += size
	}
	if len(current) > 0 {
		batches = append(batches, current)
	}
	return batches
}

// chunkBytes returns the budget contribution of one chunk. We use
// Chunk.Size when populated (it's the canonical value set by
// ChunkByFile / wrapDiffWithHeader) and fall back to len(Diff) for
// chunks constructed by hand in tests.
func chunkBytes(c llm.Chunk) int {
	if c.Size > 0 {
		return c.Size
	}
	return len(c.Diff)
}

// chunkFileList renders the file paths in a batch as a comma-
// separated string suitable for log output. Capped at the first
// `maxChunkLogFiles` entries with an "(N more)" tail to keep log
// lines bounded when a batch contains many chunks. Today
// batchChunks emits one chunk per batch, but the batching API
// supports packing small chunks together — this helper stays
// correct if that optimization lands later.
func chunkFileList(chunks []llm.Chunk) string {
	const maxChunkLogFiles = 5
	if len(chunks) == 0 {
		return ""
	}
	files := make([]string, 0, len(chunks))
	for i, c := range chunks {
		if i >= maxChunkLogFiles {
			files = append(files, fmt.Sprintf("(%d more)", len(chunks)-maxChunkLogFiles))
			break
		}
		files = append(files, c.File)
	}
	return strings.Join(files, ",")
}

// pathIndex returns the set of file paths present in the diff.
// Used to filter out LLM-hallucinated paths.
func pathIndex(changes []gitlab.ChangeFile) map[string]bool {
	out := make(map[string]bool, len(changes))
	for _, c := range changes {
		out[c.Path()] = true
	}
	return out
}

// fileMetaIndex returns the per-file diff metadata, keyed by
// canonical path (Path() returns NewPath when present, else
// OldPath). Used by postFinding to construct the correct
// position shape for the GitLab /discussions endpoint —
// new / modified / deleted / renamed each require a different
// combination of new_path / old_path / new_line / old_line.
func fileMetaIndex(changes []gitlab.ChangeFile) map[string]gitlab.ChangeFile {
	out := make(map[string]gitlab.ChangeFile, len(changes))
	for _, c := range changes {
		out[c.Path()] = c
	}
	return out
}

// countDiffLines returns the total number of newline characters
// across all change diffs. Used to decide whether the LLM's zero
// findings output is suspicious — a "looks clean" reply on a
// 5-line diff is normal; on a 500-line diff it's almost always a
// regression. Newlines are a cheap, format-agnostic proxy for diff
// size; counting `+` / `-` lines would require parsing and isn't
// worth it for a sanity check.
func countDiffLines(changes []gitlab.ChangeFile) int {
	n := 0
	for _, c := range changes {
		n += strings.Count(c.Diff, "\n")
	}
	return n
}
