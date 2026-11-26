// Package rootbilly is a go-billy filesystem over an fsops.VolumeFS: every
// path go-git reads or writes is walked one verified handle at a time from
// the volume root, and no symlink is ever followed.
//
// It lives beside gitsource, in the layer every package may import, so the
// core's Git tool and a workspace worker's supervisor (RFC GW P11) open
// repositories through the same confinement. It decides nothing about who
// may reach a volume: the caller hands it one already opened.
package rootbilly
