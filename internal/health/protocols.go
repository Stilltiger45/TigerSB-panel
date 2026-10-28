package health

import (
	"context"
	"fmt"
	"strings"

	"github.com/Stilltiger45/TigerSB-panel/internal/service"
	"github.com/Stilltiger45/TigerSB-panel/internal/singbox"
	"github.com/Stilltiger45/TigerSB-panel/internal/site"
	"github.com/Stilltiger45/TigerSB-panel/internal/xray"
)

// port is where a protocol must be listening.
type port struct {
	n   int
	udp bool
}

func (p port) String() string {
	if p.udp {
		return fmt.Sprintf("%d/udp", p.n)
	}
	return fmt.Sprintf("%d/tcp", p.n)
}

// Protocols is the state of each protocol right now, for the Users and
// Overview tabs: on or off, its core running, the domain and certificate it
// needs, its ports listening. Unlike Run it stores nothing.
func Protocols(ctx context.Context, e Env) ([]Result, error) {
	e.defaults()
	set, err := site.Load(ctx, e.Store, e.Cfg)
	if err != nil {
		return nil, err
	}
	xs, err := xray.Load(ctx, e.Store)
	if err != nil {
		return nil, err
	}
	sb, err := singbox.Load(ctx, e.Store)
	if err != nil {
		return nil, err
	}
	units := map[string]string{}
	unit := func(name string) string {
		if _, ok := units[name]; !ok {
			units[name] = service.UnitState(e.Run, name)
		}
		return units[name]
	}
	// one checks a protocol: public are the ports clients use, listen the
	// ones that must be open for it.
	one := func(id, title string, on bool, core string, cert bool, public, listen []port) Result {
		r := Result{ID: id, Title: title}
		var where []string
		for _, p := range public {
			where = append(where, p.String())
		}
		switch {
		case !on:
			r.State, r.Detail = Off, "Выключен."
		case unit(core) != "active":
			r.State, r.Detail = Fail, fmt.Sprintf("Контейнер не работает: journalctl -u %s", core)
		case cert && set.Domain == "":
			r.State, r.Detail = Warn, "Ждёт домен: задайте его на вкладке «Сайт и сертификат»."
		case cert && !e.TUICCert():
			r.State, r.Detail = Warn, "Ждёт сертификат домена."
		default:
			for _, p := range listen {
				listening := e.Listening
				if p.udp {
					listening = e.ListeningUDP
				}
				if !listening(p.n) {
					r.State, r.Detail = Fail, "Не слушает "+p.String()+"."
					return r
				}
			}
			r.State, r.Detail = OK, "Работает: "+strings.Join(where, ", ")+"."
		}
		return r
	}
	xrayPort := func(n, innerPort int) ([]port, []port) {
		if n == xray.EdgePort {
			// The edge holds 443 for the site too and has its own check;
			// Xray listens behind it.
			return []port{{n: n}}, []port{{n: innerPort}}
		}
		return []port{{n: n}}, []port{{n: n}}
	}
	gp, gl := xrayPort(xs.GRPC.Port, xray.GRPCInner)
	xp, xl := xrayPort(xs.XHTTP.Port, xray.XHTTPInner)
	tp := []port{{n: sb.TUIC.Port, udp: true}}
	hp := []port{{n: sb.Hysteria2.Port, udp: true}}
	n := sb.Naive
	// NaïveProxy over TCP needs the edge on its port and sing-box behind it.
	var np, nl []port
	if n.TCP() {
		np, nl = append(np, port{n: n.Port}), append(nl, port{n: n.Port}, port{n: xray.NaiveInner})
	}
	if n.QUIC() {
		np, nl = append(np, port{n: n.Port, udp: true}), append(nl, port{n: n.Port, udp: true})
	}
	m := sb.Mieru
	mp := port{n: m.PortFrom, udp: m.Transport == "UDP"}
	mieru := one(singbox.MieruTag, "Mieru", m.Enabled, singbox.MieruUnit, false, []port{mp}, []port{mp})
	if mieru.State == OK && m.PortTo > m.PortFrom {
		mieru.Detail = fmt.Sprintf("Работает: %d-%d/%s.", m.PortFrom, m.PortTo, strings.ToLower(m.Transport))
	}
	return []Result{
		one(xray.XHTTPTag, "XHTTP", xs.XHTTP.Enabled, xray.Unit, false, xp, xl),
		one(xray.GRPCTag, "gRPC", xs.GRPC.Enabled, xray.Unit, false, gp, gl),
		one(singbox.TUICTag, "TUIC", sb.TUIC.Enabled, singbox.Unit, true, tp, tp),
		one(singbox.Hy2Tag, "Hysteria 2", sb.Hysteria2.Enabled, singbox.Unit, true, hp, hp),
		one(singbox.NaiveTag, "NaïveProxy", n.Enabled, singbox.Unit, true, np, nl),
		mieru,
	}, nil
}
