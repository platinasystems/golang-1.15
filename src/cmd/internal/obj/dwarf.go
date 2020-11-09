// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Writes dwarf information to object files.

package obj

import (
	"cmd/internal/dwarf"
	"cmd/internal/src"
	"fmt"
)

// Generate a sequence of opcodes that is as short as possible.
// See section 6.2.5
const (
	LINE_BASE   = -4
	LINE_RANGE  = 10
	PC_RANGE    = (255 - OPCODE_BASE) / LINE_RANGE
	OPCODE_BASE = 11
)

// generateDebugLinesSymbol fills the debug lines symbol of a given function.
//
// It's worth noting that this function doesn't generate the full debug_lines
// DWARF section, saving that for the linker. This function just generates the
// state machine part of debug_lines. The full table is generated by the
// linker.  Also, we use the file numbers from the full package (not just the
// function in question) when generating the state machine. We do this so we
// don't have to do a fixup on the indices when writing the full section.
func (ctxt *Link) generateDebugLinesSymbol(s, lines *LSym) {
	dctxt := dwCtxt{ctxt}

	// The Pcfile table is used to generate the debug_lines section, and the file
	// indices for that data could differ from the files we write out for the
	// debug_lines section. Here we generate a LUT between those two indices.
	fileNums := make(map[int32]int64)
	for i, filename := range s.Func.Pcln.File {
		if symbolIndex := ctxt.PosTable.FileIndex(filename); symbolIndex >= 0 {
			fileNums[int32(i)] = int64(symbolIndex) + 1
		} else {
			panic(fmt.Sprintf("First time we've seen filename: %q", filename))
		}
	}

	// Set up the debug_lines state machine to the default values
	// we expect at the start of a new sequence.
	stmt := true
	line := int64(1)
	pc := s.Func.Text.Pc
	var lastpc int64 // last PC written to line table, not last PC in func
	name := ""
	prologue, wrotePrologue := false, false
	// Walk the progs, generating the DWARF table.
	for p := s.Func.Text; p != nil; p = p.Link {
		prologue = prologue || (p.Pos.Xlogue() == src.PosPrologueEnd)
		// If we're not at a real instruction, keep looping!
		if p.Pos.Line() == 0 || (p.Link != nil && p.Link.Pc == p.Pc) {
			continue
		}
		newStmt := p.Pos.IsStmt() != src.PosNotStmt
		newName, newLine := linkgetlineFromPos(ctxt, p.Pos)

		// Output debug info.
		wrote := false
		if name != newName {
			newFile := ctxt.PosTable.FileIndex(newName) + 1 // 1 indexing for the table.
			dctxt.AddUint8(lines, dwarf.DW_LNS_set_file)
			dwarf.Uleb128put(dctxt, lines, int64(newFile))
			name = newName
			wrote = true
		}
		if prologue && !wrotePrologue {
			dctxt.AddUint8(lines, uint8(dwarf.DW_LNS_set_prologue_end))
			wrotePrologue = true
			wrote = true
		}
		if stmt != newStmt {
			dctxt.AddUint8(lines, uint8(dwarf.DW_LNS_negate_stmt))
			stmt = newStmt
			wrote = true
		}

		if line != int64(newLine) || wrote {
			pcdelta := p.Pc - pc
			lastpc = p.Pc
			putpclcdelta(ctxt, dctxt, lines, uint64(pcdelta), int64(newLine)-line)
			line, pc = int64(newLine), p.Pc
		}
	}

	// Because these symbols will be concatenated together by the
	// linker, we need to reset the state machine that controls the
	// debug symbols. Do this using an end-of-sequence operator.
	//
	// Note: at one point in time, Delve did not support multiple end
	// sequence ops within a compilation unit (bug for this:
	// https://github.com/go-delve/delve/issues/1694), however the bug
	// has since been fixed (Oct 2019).
	//
	// Issue 38192: the DWARF standard specifies that when you issue
	// an end-sequence op, the PC value should be one past the last
	// text address in the translation unit, so apply a delta to the
	// text address before the end sequence op. If this isn't done,
	// GDB will assign a line number of zero the last row in the line
	// table, which we don't want.
	lastlen := uint64(s.Size - (lastpc - s.Func.Text.Pc))
	putpclcdelta(ctxt, dctxt, lines, lastlen, 0)
	dctxt.AddUint8(lines, 0) // start extended opcode
	dwarf.Uleb128put(dctxt, lines, 1)
	dctxt.AddUint8(lines, dwarf.DW_LNE_end_sequence)
}

func putpclcdelta(linkctxt *Link, dctxt dwCtxt, s *LSym, deltaPC uint64, deltaLC int64) {
	// Choose a special opcode that minimizes the number of bytes needed to
	// encode the remaining PC delta and LC delta.
	var opcode int64
	if deltaLC < LINE_BASE {
		if deltaPC >= PC_RANGE {
			opcode = OPCODE_BASE + (LINE_RANGE * PC_RANGE)
		} else {
			opcode = OPCODE_BASE + (LINE_RANGE * int64(deltaPC))
		}
	} else if deltaLC < LINE_BASE+LINE_RANGE {
		if deltaPC >= PC_RANGE {
			opcode = OPCODE_BASE + (deltaLC - LINE_BASE) + (LINE_RANGE * PC_RANGE)
			if opcode > 255 {
				opcode -= LINE_RANGE
			}
		} else {
			opcode = OPCODE_BASE + (deltaLC - LINE_BASE) + (LINE_RANGE * int64(deltaPC))
		}
	} else {
		if deltaPC <= PC_RANGE {
			opcode = OPCODE_BASE + (LINE_RANGE - 1) + (LINE_RANGE * int64(deltaPC))
			if opcode > 255 {
				opcode = 255
			}
		} else {
			// Use opcode 249 (pc+=23, lc+=5) or 255 (pc+=24, lc+=1).
			//
			// Let x=deltaPC-PC_RANGE.  If we use opcode 255, x will be the remaining
			// deltaPC that we need to encode separately before emitting 255.  If we
			// use opcode 249, we will need to encode x+1.  If x+1 takes one more
			// byte to encode than x, then we use opcode 255.
			//
			// In all other cases x and x+1 take the same number of bytes to encode,
			// so we use opcode 249, which may save us a byte in encoding deltaLC,
			// for similar reasons.
			switch deltaPC - PC_RANGE {
			// PC_RANGE is the largest deltaPC we can encode in one byte, using
			// DW_LNS_const_add_pc.
			//
			// (1<<16)-1 is the largest deltaPC we can encode in three bytes, using
			// DW_LNS_fixed_advance_pc.
			//
			// (1<<(7n))-1 is the largest deltaPC we can encode in n+1 bytes for
			// n=1,3,4,5,..., using DW_LNS_advance_pc.
			case PC_RANGE, (1 << 7) - 1, (1 << 16) - 1, (1 << 21) - 1, (1 << 28) - 1,
				(1 << 35) - 1, (1 << 42) - 1, (1 << 49) - 1, (1 << 56) - 1, (1 << 63) - 1:
				opcode = 255
			default:
				opcode = OPCODE_BASE + LINE_RANGE*PC_RANGE - 1 // 249
			}
		}
	}
	if opcode < OPCODE_BASE || opcode > 255 {
		panic(fmt.Sprintf("produced invalid special opcode %d", opcode))
	}

	// Subtract from deltaPC and deltaLC the amounts that the opcode will add.
	deltaPC -= uint64((opcode - OPCODE_BASE) / LINE_RANGE)
	deltaLC -= (opcode-OPCODE_BASE)%LINE_RANGE + LINE_BASE

	// Encode deltaPC.
	if deltaPC != 0 {
		if deltaPC <= PC_RANGE {
			// Adjust the opcode so that we can use the 1-byte DW_LNS_const_add_pc
			// instruction.
			opcode -= LINE_RANGE * int64(PC_RANGE-deltaPC)
			if opcode < OPCODE_BASE {
				panic(fmt.Sprintf("produced invalid special opcode %d", opcode))
			}
			dctxt.AddUint8(s, dwarf.DW_LNS_const_add_pc)
		} else if (1<<14) <= deltaPC && deltaPC < (1<<16) {
			dctxt.AddUint8(s, dwarf.DW_LNS_fixed_advance_pc)
			dctxt.AddUint16(s, uint16(deltaPC))
		} else {
			dctxt.AddUint8(s, dwarf.DW_LNS_advance_pc)
			dwarf.Uleb128put(dctxt, s, int64(deltaPC))
		}
	}

	// Encode deltaLC.
	if deltaLC != 0 {
		dctxt.AddUint8(s, dwarf.DW_LNS_advance_line)
		dwarf.Sleb128put(dctxt, s, deltaLC)
	}

	// Output the special opcode.
	dctxt.AddUint8(s, uint8(opcode))
}
