package history

import "github.com/joeycumines/ai-concurrency-shaper/internal/observation"

// packetBytes conservatively accounts owned structs, slice backing arrays and
// string contents. It performs no encoding or I/O. Shared string storage can be
// counted more than once; the budget is a bound, not an RSS measurement.
func packetBytes(p observation.Packet) uint64 {
	n := uint64(128) + uint64(cap(p.Intervals))*2048 + uint64(cap(p.Requests))*768 + uint64(cap(p.Markers))*128
	for _, v := range p.Intervals {
		n += uint64(len(v.Epoch)+len(v.Provider)) + uint64(cap(v.Resources))*256
		for _, r := range v.Resources {
			n += uint64(len(r.Provider) + len(r.ID) + len(r.Kind))
		}
	}
	for _, f := range p.Requests {
		n += uint64(cap(f.Attempts))*128 + uint64(len(f.ID)+len(f.Epoch)+len(f.Provider)+len(f.ReceivedProvider)+len(f.Method)+len(f.Path)+len(f.Route)+len(f.ClientProtocol)+len(f.UpstreamProtocol)+len(f.Model)+len(f.Fingerprint)+len(f.Outcome)+len(f.Stage))
		for _, a := range f.Attempts {
			n += uint64(len(a.Provider) + len(a.Kind) + len(a.Outcome))
		}
		if f.Usage != nil {
			n += 64 + uint64(len(f.Usage.Source))
		}
	}
	for _, m := range p.Markers {
		n += uint64(len(m.ID) + len(m.Provider) + len(m.Kind) + len(m.Summary) + len(m.RequestID))
	}
	return n
}
