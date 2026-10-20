package usages

// reportStateNotices turns what the refresh state store wants reported (a save
// that failed, a state file set aside) into the refresher's alerts. It is called
// at the top of every due pass, so a notice raised by any write since the last
// pass is announced within one tick.
func (r *TargetRefresher) reportStateNotices() {
	for _, n := range r.stateStore().drainNotices() {
		r.emit(n.Kind, "refresh-state", n.Detail)
	}
}
