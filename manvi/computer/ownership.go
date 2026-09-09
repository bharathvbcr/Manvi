package computer

func ownedSelectors(values []Selector) []Selector {
	out := append([]Selector(nil), values...)
	for i := range out {
		if out[i].Visual != nil {
			v := *out[i].Visual
			out[i].Visual = &v
		}
	}
	return out
}

func ownedAction(a Action) Action {
	if a.Text != nil {
		v := *a.Text
		a.Text = &v
	}
	if a.X != nil {
		v := *a.X
		a.X = &v
	}
	if a.Y != nil {
		v := *a.Y
		a.Y = &v
	}
	if a.ScrollY != nil {
		v := *a.ScrollY
		a.ScrollY = &v
	}
	return a
}

func ownedRecord(r Record) Record {
	if r.Observation != nil {
		o := ownedObservation(*r.Observation)
		r.Observation = &o
	}
	if r.Event != nil {
		e := *r.Event
		e.Observation = e.Observation.Clone()
		r.Event = &e
	}
	if r.Receipt != nil {
		receipt := *r.Receipt
		r.Receipt = &receipt
	}
	return r
}
