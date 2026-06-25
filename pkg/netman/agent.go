package netman

import (
	"log"

	"github.com/godbus/dbus/v5"
)

// iwdAgent implements the net.connman.iwd.Agent interface. iwd invokes
// RequestPassphrase whenever a connection attempt needs credentials we have not
// already provided (i.e. a secured network that is not yet known).
type iwdAgent struct {
	requestPassphrase func(network dbus.ObjectPath) (string, *dbus.Error)
}

func (a *iwdAgent) RequestPassphrase(network dbus.ObjectPath) (string, *dbus.Error) {
	return a.requestPassphrase(network)
}

func (a *iwdAgent) Release() *dbus.Error { return nil }

func (a *iwdAgent) Cancel(reason string) *dbus.Error {
	log.Println("Passphrase request cancelled:", reason)
	return nil
}
