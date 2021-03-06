package instruction

import (
	"io"

	"github.com/chrislusf/gleam/msg"
)

var (
	InstructionRunner = &instructionRunner{}
)

type Order int

const (
	Ascending  = Order(1)
	Descending = Order(-1)
)

type OrderBy struct {
	Index int   // column index, starting from 1
	Order Order // Ascending or Descending
}

type Stats struct {
	Count int
}

type Instruction interface {
	Name() string
	Function() func(readers []io.Reader, writers []io.Writer, stats *Stats) error
	SerializeToCommand() *msg.Instruction
	GetMemoryCostInMB(partitionSize int64) int64
}

type instructionRunner struct {
	functions []func(*msg.Instruction) Instruction
}

func (r *instructionRunner) Register(f func(*msg.Instruction) Instruction) {
	r.functions = append(r.functions, f)
}

func (r *instructionRunner) GetInstructionFunction(i *msg.Instruction) func(readers []io.Reader, writers []io.Writer, stats *Stats) error {
	for _, f := range r.functions {
		if inst := f(i); inst != nil {
			return inst.Function()
		}
	}
	return nil
}
