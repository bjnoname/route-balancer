package actor

type Mailbox[M any] struct{ ch chan M }

func NewMailbox[M any](depth int) *Mailbox[M] { return &Mailbox[M]{ch: make(chan M, depth)} }

func (m *Mailbox[M]) receiver() <-chan M { return m.ch }

func (m *Mailbox[M]) Len() int { return len(m.ch) }
