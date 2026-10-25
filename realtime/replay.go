package realtime

// replay keeps the latest events across topics, in the order the hub
// delivered them, so a client that reconnects can resume after the last
// event it saw. It is guarded by the Hub's lock.
type replay struct {
	buf   []*frame
	head  int    // index of the oldest event
	n     int    // events kept
	first uint64 // position of the oldest event in the hub's sequence
	pos   map[string]uint64
}

func newReplay(size int) *replay {
	return &replay{buf: make([]*frame, size), pos: make(map[string]uint64, size)}
}

// add keeps f, evicting the oldest event when full.
func (r *replay) add(f *frame) {
	if len(r.buf) == 0 {
		return
	}
	if r.n == len(r.buf) {
		old := r.buf[r.head]
		if r.pos[old.id] == r.first {
			delete(r.pos, old.id)
		}
		r.buf[r.head] = nil
		r.head = (r.head + 1) % len(r.buf)
		r.first++
		r.n--
	}
	r.buf[(r.head+r.n)%len(r.buf)] = f
	r.pos[f.id] = r.first + uint64(r.n)
	r.n++
}

// since returns the events after the one with id that went to topics,
// oldest first, and whether that event is still kept.
func (r *replay) since(id string, topics []string) ([]*frame, bool) {
	p, ok := r.pos[id]
	if !ok {
		return nil, false
	}
	want := make(map[string]bool, len(topics))
	for _, t := range topics {
		want[t] = true
	}
	var out []*frame
	for q := p + 1; q < r.first+uint64(r.n); q++ {
		if f := r.buf[(r.head+int(q-r.first))%len(r.buf)]; want[f.topic] {
			out = append(out, f)
		}
	}
	return out, true
}
