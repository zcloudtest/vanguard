package xfr

import (
	"github.com/zdnscloud/vanguard/core"
	"github.com/zdnscloud/vanguard/resolver/auth"
	"github.com/zdnscloud/vanguard/viewselector"
)

type XFRHandler struct {
	core.DefaultHandler

	viewSelector *viewselector.SelectorMgr
	runner       *XFRRunner
}

func NewXFRHandler(viewselector *viewselector.SelectorMgr, auth *auth.AuthDataSource) *XFRHandler {
	return &XFRHandler{
		viewSelector: viewselector,
		runner:       newXFRRunner(auth),
	}
}

func (h *XFRHandler) HandleQuery(ctx *core.Context) {
	if h.viewSelector.SelectView(ctx) {
		h.runner.HandleNotify(ctx)
	}
}
