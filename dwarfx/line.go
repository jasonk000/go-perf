// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package dwarfx

import (
	"debug/dwarf"
	"encoding/binary"
	"errors"
	"fmt"
	"path"
)

// A LineReader allows reading a sequence of LineEntry structures from
// a DWARF "line" section for a single compile unit.  LineEntries
// occur in order of increasing PC and each LineEntry gives metadata
// for the instructions from that LineEntry's PC to just before the
// next LineEntry's PC.  The last entry will have its EndSequence
// field set.
type LineReader struct {
	buf buf

	// Original .debug_line section data.  Used by Seek.
	section []byte

	// Prologue information
	version              uint16
	minInstructionLength int
	maxOpsPerInstruction int
	defaultIsStmt        bool
	lineBase             int
	lineRange            int
	opcodeBase           int
	opcodeLengths        []int
	directories          []string
	fileEntries          []*FileEntry

	endOffset     dwarf.Offset // section offset of byte following program
	programOffset dwarf.Offset // section offset of statement program

	initialFileEntries int // initial length of fileEntries

	// Current "statement machine" state
	state LineEntry
}

// A LineEntry is a row in a DWARF line table.
type LineEntry struct {
	// The program-counter value corresponding to a machine
	// instruction generated by the compiler.  This LineEntry
	// applies to each instruction from Address to just before the
	// Address of the next LineEntry.
	Address uint64

	// The index of an operation within a VLIW instruction.  The
	// index of the first operation is 0.  For non-VLIW
	// architectures, this will always be 0.  Address and OpIndex
	// together form an operation pointer that can reference any
	// individual operation with the instruction stream.
	OpIndex int

	// An index indicating the identity of the source file
	// corresponding to these instructions.
	//
	// TODO: Make this private
	FileIndex int

	// The source file corresponding to these instructions.
	FileEntry *FileEntry

	// The line number of the source code corresponding to these
	// instructions.  Lines are numbered beginning at 1.  This may
	// be 0 if these instructions cannot be attributed to any
	// source line.
	Line int

	// The column number within the source line.  Columns are
	// numbered beginning at 1.  This may be 0 to indicate the
	// "left edge" of the line.
	Column int

	// IsStmt indicates that Address is a recommended breakpoint
	// location, such as the beginning of a line, statement, or a
	// distinct subpart of a statement.
	IsStmt bool

	// BasicBlock indicates that Address is the beginning of a
	// basic block.
	BasicBlock bool

	// PrologueEnd indicates that Address is one (of possibly
	// many) PCs where execution should be suspended for a
	// breakpoint on entry to the containing function.
	//
	// Added in DWARF 3.
	PrologueEnd bool

	// EpilogueBegin indicates that Address is one (of possibly
	// many) PCs where execution should be suspended for a
	// breakpoint on exit from this function.
	//
	// Added in DWARF 3.
	EpilogueBegin bool

	// The instruction set architecture for the instructions
	// covered by this LineEntry.  Possible values for this should
	// be defined by the applicable ABI specification.
	//
	// Added in DWARF 3.
	ISA int

	// An arbitrary integer indicating the block to which these
	// instructions belong.  This serves to distinguish among
	// multiple blocks that may all have with the same source
	// file, line, and column.  Where only one block exists for a
	// given source position, this should be 0.
	//
	// Added in DWARF 3.
	Discriminator int

	// EndSequence indicates that Address is the first byte after
	// the end of a sequence of target machine instructions.  If
	// this is set, only this and the Address field are
	// meaningful.  A line number table may contain information
	// for multiple potentially disjoint instruction sequences.
	// The last entry in a line table should always have
	// EndSequence set.
	EndSequence bool
}

// A FileEntry represents a source file referenced by a DWARF line
// table entry.
type FileEntry struct {
	FileName string
	Mtime    uint64 // Modification time, or 0 if unknown
	Length   int    // File length, or 0 if unknown
}

type dwarf64Format struct{}

func (dwarf64Format) version() int {
	return 0
}

func (dwarf64Format) dwarf64() (bool, bool) {
	return true, true
}

func (dwarf64Format) addrsize() int {
	return 8
}

// NewLineReader returns a new reader for the line table of
// compilation unit cu.
//
// Line tables are per-compilation unit.  cu must be an Entry with tag
// TagCompileUnit.  line must be the contents of the .debug_line
// section of the corresponding ELF file.
//
// If this compilation unit has no line table, this returns nil, nil.
func NewLineReader(cu *dwarf.Entry, line []byte) (*LineReader, error) {
	off, ok := cu.Val(dwarf.AttrStmtList).(int64)
	if !ok {
		// cu has no line table
		return nil, nil
	}
	compDir, _ := cu.Val(dwarf.AttrCompDir).(string)

	if off > int64(len(line)) {
		off = int64(len(line))
	}

	// TODO: Use correct byte order and format.  The dwarf package
	// hides this information and it's annoying to dig out
	// ourselves.
	buf := makeBuf(nil, binary.LittleEndian, dwarf64Format{}, "line", dwarf.Offset(off), line[off:])

	// The compilation directory is implicitly directories[0]
	r := LineReader{buf: buf, section: line, directories: []string{compDir}}

	// Read the prologue/header and initialize the state machine
	if err := r.readPrologue(); err != nil {
		return nil, err
	}

	// Initialize line reader state
	r.Reset()

	return &r, nil
}

// readPrologue reads the statement program prologue from r.buf and
// sets all of the prologue fields in r.
func (r *LineReader) readPrologue() error {
	buf := &r.buf

	// Read basic prologue fields [DWARF2 6.2.4]
	hdrOffset := buf.off
	totalLength := dwarf.Offset(buf.uint32())
	r.endOffset = hdrOffset + totalLength + 4
	if r.endOffset > buf.off+dwarf.Offset(len(buf.data)) {
		return DecodeError{"line", hdrOffset, fmt.Sprintf("line table end %d exceeds section size %d", r.endOffset, buf.off+dwarf.Offset(len(buf.data)))}
	}
	r.version = buf.uint16()
	if buf.err == nil && (r.version < 2 || r.version > 4) {
		// DWARF goes to all this effort to make new opcodes
		// backward-compatible, and then adds fields right in
		// the middle of the prologue in new versions, so
		// we're picky about only supporting known line table
		// versions.
		return DecodeError{"line", hdrOffset, fmt.Sprintf("unknown line table version %d", r.version)}
	}
	prologueLength := dwarf.Offset(buf.uint32())
	r.programOffset = buf.off + prologueLength
	r.minInstructionLength = int(buf.uint8())
	if r.version >= 4 {
		// [DWARF4 6.2.4]
		r.maxOpsPerInstruction = int(buf.uint8())
	} else {
		r.maxOpsPerInstruction = 1
	}
	r.defaultIsStmt = (buf.uint8() != 0)
	r.lineBase = int(int8(buf.uint8()))
	r.lineRange = int(buf.uint8())

	// Validate header
	if buf.err != nil {
		return buf.err
	}
	if r.maxOpsPerInstruction == 0 {
		return DecodeError{"line", hdrOffset, "invalid maximum operations per instruction: 0"}
	}
	if r.lineRange == 0 {
		return DecodeError{"line", hdrOffset, "invalid line range: 0"}
	}

	// Read opcode length table
	r.opcodeBase = int(buf.uint8())
	r.opcodeLengths = make([]int, r.opcodeBase)
	for i := 1; i < r.opcodeBase; i++ {
		r.opcodeLengths[i] = int(buf.uint8())
	}

	// Validate opcode lengths
	if buf.err != nil {
		return buf.err
	}
	for i, length := range r.opcodeLengths {
		if known, ok := knownOpcodeLengths[i]; ok && known != length {
			return DecodeError{"line", hdrOffset, fmt.Sprintf("opcode %d expected to have length %d, but has length %d", i, known, length)}
		}
	}

	// Read include directories table.  The caller already set
	// directories[0] to the compilation directory.
	for {
		directory := buf.string()
		if buf.err != nil {
			return buf.err
		}
		if len(directory) == 0 {
			break
		}
		if !path.IsAbs(directory) {
			// Relative paths are implicitly relative to
			// the compilation directory.
			directory = path.Join(r.directories[0], directory)
		}
		r.directories = append(r.directories, directory)
	}

	// Read file name list.  File numbering starts with 1, so
	// leave the first entry nil.
	r.fileEntries = make([]*FileEntry, 1)
	for {
		if done, err := r.readFileEntry(); err != nil {
			return err
		} else if done {
			break
		}
	}
	r.initialFileEntries = len(r.fileEntries)

	return buf.err
}

// readFileEntry reads a file entry from either the prologue or a
// DW_LNE_define_file extended opcode and adds it to r.fileEntries.  A
// true return value indicates that there are no more entries to read.
func (r *LineReader) readFileEntry() (bool, error) {
	name := r.buf.string()
	if r.buf.err != nil {
		return false, r.buf.err
	}
	if len(name) == 0 {
		return true, nil
	}
	off := r.buf.off
	dirIndex := int(r.buf.uint())
	if !path.IsAbs(name) {
		if dirIndex >= len(r.directories) {
			return false, DecodeError{"line", off, "directory index too large"}
		}
		name = path.Join(r.directories[dirIndex], name)
	}
	mtime := r.buf.uint()
	length := int(r.buf.uint())

	r.fileEntries = append(r.fileEntries, &FileEntry{name, mtime, length})
	return false, nil
}

// updateFileEntry updates r.state.FileEntry after r.state.FileIndex
// has changed or r.fileEntries has changed.
func (r *LineReader) updateFileEntry() {
	if r.state.FileIndex < len(r.fileEntries) {
		r.state.FileEntry = r.fileEntries[r.state.FileIndex]
	} else {
		r.state.FileEntry = nil
	}
}

// EndOfTable is the error returned by LineReader.Next when no more
// line table entries are available.  This signals a graceful end of
// the table.
var EndOfTable = errors.New("EndOfTable")

// Next sets *entry to the next row in this line table and moves to
// the next row.  If there are no more entries and the line table is
// properly terminated, it returns EndOfTable.
//
// Rows are always in order of increasing Address, but Line may go
// forward or backward.
func (r *LineReader) Next(entry *LineEntry) error {
	if r.buf.err != nil {
		return r.buf.err
	}

	// Execute opcodes until we reach an opcode that emits a line
	// table entry
	for {
		if len(r.buf.data) == 0 {
			return EndOfTable
		}
		emit := r.step(entry)
		if r.buf.err != nil {
			return r.buf.err
		}
		if emit {
			return nil
		}
	}
}

// knownOpcodeLengths gives the opcode lengths (in varint arguments)
// of known standard opcodes.
var knownOpcodeLengths = map[int]int{
	lnsCopy:             0,
	lnsAdvancePC:        1,
	lnsAdvanceLine:      1,
	lnsSetFile:          1,
	lnsNegateStmt:       0,
	lnsSetBasicBlock:    0,
	lnsConstAddPC:       0,
	lnsSetPrologueEnd:   0,
	lnsSetEpilogueBegin: 0,
	lnsSetISA:           1,
	// lnsFixedAdvancePC takes a uint8 rather than a varint; it's
	// unclear what length the header is supposed to claim, so
	// ignore it.
}

// step processes the next opcode and updates r.state.  If the opcode
// emits a row in the line table, this updates *entry and returns
// true.
func (r *LineReader) step(entry *LineEntry) bool {
	opcode := int(r.buf.uint8())

	if opcode >= r.opcodeBase {
		// Special opcode [DWARF2 6.2.5.1, DWARF4 6.2.5.1]
		adjustedOpcode := opcode - r.opcodeBase
		r.advancePC(adjustedOpcode / r.lineRange)
		lineDelta := r.lineBase + int(adjustedOpcode)%r.lineRange
		r.state.Line += lineDelta
		goto emit
	}

	switch opcode {
	case 0:
		// Extended opcode [DWARF2 6.2.5.3]
		length := dwarf.Offset(r.buf.uint())
		startOff := r.buf.off
		opcode := r.buf.uint8()

		switch opcode {
		case lneEndSequence:
			r.state.EndSequence = true
			*entry = r.state
			r.resetState()

		case lneSetAddress:
			r.state.Address = r.buf.addr()

		case lneDefineFile:
			if done, err := r.readFileEntry(); err != nil {
				r.buf.err = err
				return false
			} else if done {
				r.buf.err = DecodeError{"line", startOff, "malformed DW_LNE_define_file operation"}
				return false
			}
			r.updateFileEntry()

		case lneSetDiscriminator:
			// [DWARF4 6.2.5.3]
			r.state.Discriminator = int(r.buf.uint())
		}

		r.buf.skip(int(startOff + length - r.buf.off))

		if opcode == lneEndSequence {
			return true
		}

	// Standard opcodes [DWARF2 6.2.5.2]
	case lnsCopy:
		goto emit

	case lnsAdvancePC:
		r.advancePC(int(r.buf.uint()))

	case lnsAdvanceLine:
		r.state.Line += int(r.buf.int())

	case lnsSetFile:
		r.state.FileIndex = int(r.buf.uint())
		r.updateFileEntry()

	case lnsSetColumn:
		r.state.Column = int(r.buf.uint())

	case lnsNegateStmt:
		r.state.IsStmt = !r.state.IsStmt

	case lnsSetBasicBlock:
		r.state.BasicBlock = true

	case lnsConstAddPC:
		r.advancePC((255 - r.opcodeBase) / r.lineRange)

	case lnsFixedAdvancePC:
		r.state.Address += uint64(r.buf.uint16())

	// DWARF3 standard opcodes [DWARF3 6.2.5.2]
	case lnsSetPrologueEnd:
		r.state.PrologueEnd = true

	case lnsSetEpilogueBegin:
		r.state.EpilogueBegin = true

	case lnsSetISA:
		r.state.ISA = int(r.buf.uint())

	default:
		// Unhandled standard opcode.  Skip the number of
		// arguments that the prologue says this opcode has.
		for i := 0; i < r.opcodeLengths[opcode]; i++ {
			r.buf.uint()
		}
	}
	return false

emit:
	*entry = r.state
	r.state.BasicBlock = false
	r.state.PrologueEnd = false
	r.state.EpilogueBegin = false
	r.state.Discriminator = 0
	return true
}

// advancePC advances "operation pointer" (the combination of Address
// and OpIndex) in r.state by opAdvance steps.
func (r *LineReader) advancePC(opAdvance int) {
	opIndex := r.state.OpIndex + opAdvance
	r.state.Address += uint64(r.minInstructionLength * (opIndex / r.maxOpsPerInstruction))
	r.state.OpIndex = opIndex % r.maxOpsPerInstruction
}

// A LineReaderPos represents a position in a line table.
type LineReaderPos struct {
	// Current offset in the DWARF line section
	off dwarf.Offset
	// Length of fileEntries
	numFileEntries int
	// Statement machine state at this offset
	state LineEntry
}

// Tell returns the current position in the line table.
func (r *LineReader) Tell() LineReaderPos {
	return LineReaderPos{r.buf.off, len(r.fileEntries), r.state}
}

// Seek restores the line table reader to a position returned by Tell.
//
// pos must have been returned by a call to Tell on this line table.
func (r *LineReader) Seek(pos LineReaderPos) {
	r.buf.off = pos.off
	r.buf.data = r.section[r.buf.off:r.endOffset]
	r.fileEntries = r.fileEntries[:pos.numFileEntries]
	r.state = pos.state
}

// Reset repositions the line table reader at the beginning of the
// line table.
func (r *LineReader) Reset() {
	// Reset buffer to the program offset
	r.buf.off = r.programOffset
	r.buf.data = r.section[r.buf.off:r.endOffset]

	// Reset file entries list
	r.fileEntries = r.fileEntries[:r.initialFileEntries]

	// Reset statement program state
	r.resetState()
}

// resetState resets r.state to its default values
func (r *LineReader) resetState() {
	r.state = LineEntry{
		Address:       0,
		OpIndex:       0,
		FileIndex:     1,
		FileEntry:     nil,
		Line:          1,
		Column:        0,
		IsStmt:        r.defaultIsStmt,
		BasicBlock:    false,
		PrologueEnd:   false,
		EpilogueBegin: false,
		ISA:           0,
		Discriminator: 0,
	}
	r.updateFileEntry()
}

// UnknownPC is the error returned by ScanPC when the seek PC is not
// covered by the line table.
var UnknownPC = errors.New("UnknownPC")

// SeekPC sets *entry to the LineEntry that includes pc and positions
// the reader on the next entry in the line table.  If necessary, this
// will seek backwards to find pc.
//
// If pc is not covered by any entry in this line table, SeekPC
// returns UnknownPC.  In this case, *entry and the final seek
// position are unspecified.
//
// Note that DWARF line tables only permit sequential, forward scans.
// Hence, in the worst case, this takes linear time in the size of the
// line table.  If the caller wishes to do repeated fast PC lookups,
// it should build an appropriate index of the line table.
func (r *LineReader) SeekPC(pc uint64, entry *LineEntry) error {
	if err := r.Next(entry); err != nil {
		return err
	}
	if entry.Address > pc {
		// We're too far.  Start at the beginning of the table
		r.Reset()
		if err := r.Next(entry); err != nil {
			return err
		}
		if entry.Address > pc {
			// The whole table starts after pc
			r.Reset()
			return UnknownPC
		}
	}

	// Scan until we pass pc, then back up one
	for {
		var next LineEntry
		pos := r.Tell()
		if err := r.Next(&next); err != nil {
			if err == EndOfTable {
				return UnknownPC
			}
			return err
		}
		if next.Address > pc {
			if entry.EndSequence {
				// pc is in a hole in the table
				return UnknownPC
			}
			// entry is the desired entry.  Back up the
			// cursor to "next" and return success.
			r.Seek(pos)
			return nil
		}
		*entry = next
	}
}
