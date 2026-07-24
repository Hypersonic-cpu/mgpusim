package simdebug

import (
	"github.com/sarchlab/akita/v5/hooking"
	"github.com/sarchlab/akita/v5/messaging"
)

type connectionReader interface {
	Connection() messaging.Connection
}

type portRouteHook struct{}

func (h *portRouteHook) Func(ctx hooking.HookCtx) {
	if ctx.Pos != messaging.HookPosPortMsgSend {
		return
	}
	port, ok := ctx.Domain.(messaging.Port)
	if !ok {
		return
	}
	message, ok := ctx.Item.(messaging.Msg)
	if !ok {
		return
	}
	connectionName := "<none>"
	if reader, ok := port.(connectionReader); ok {
		if connection := reader.Connection(); connection != nil {
			connectionName = connection.Name()
		}
	}
	DPrintf(
		MemRoute,
		"src=%s dst=%s conn=%s type=%T id=%d rsp-to=%d class=%s",
		port.AsRemote(),
		message.Meta().Dst,
		connectionName,
		message,
		message.Meta().ID,
		message.Meta().RspTo,
		message.Meta().TrafficClass,
	)
}

// TracePortRoutes attaches route logging when MemRoute is enabled.
func TracePortRoutes(port messaging.Port) {
	if Enabled(MemRoute) {
		port.AcceptHook(&portRouteHook{})
	}
}
