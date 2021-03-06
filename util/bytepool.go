package util

type BytePool struct {
	c chan []byte
	w int
}

func NewBytePool(maxSize int, width int) (bp *BytePool) {
	return &BytePool{
		c: make(chan []byte, maxSize),
		w: width,
	}
}

func (bp *BytePool) Get() (b []byte) {
	select {
	case b = <-bp.c:
	default:
		b = make([]byte, bp.w)
	}
	return
}

func (bp *BytePool) Put(b []byte) {
	select {
	case bp.c <- b:
	default:
	}
}

func (bp *BytePool) Width() (n int) {
	return bp.w
}
