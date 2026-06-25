package main

import (
	"fmt"
	"os"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"
	"github.com/godbus/dbus/v5"

	"github.com/FyshOS/networks/pkg/netman"
)

func main() {
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		fmt.Println(err.Error())
		os.Exit(1)
	}

	a := app.New()
	w := a.NewWindow("Networks")

	handlePass := func(name string) string {
		result := make(chan string, 1)
		entry := widget.NewPasswordEntry()
		d := dialog.NewForm("Connect to "+name, "Connect", "Cancel",
			[]*widget.FormItem{widget.NewFormItem("Password", entry)},
			func(ok bool) {
				if ok {
					result <- entry.Text
				} else {
					result <- ""
				}
			}, w)
		d.Resize(fyne.NewSize(320, d.MinSize().Height))
		d.Show()

		return <-result
	}
	nm, err := netman.New(conn, handlePass, func(err error) {
		dialog.ShowError(err, w)
	})
	if err != nil {
		fmt.Println(err.Error())
		os.Exit(1)
	}

	w.SetContent(nm)
	w.Resize(fyne.NewSize(320, 360))
	w.ShowAndRun()
}
