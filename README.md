# SSH-Cloud

The ultimate simple cloud storage client. As simple as it gets.

Your cloud is any server you can SSH into. No sync engine, no daemon, no
database, no vendor. Just a Windows window on a folder on your server.

- **Browse** the server like Windows Explorer: rename, move, copy and delete
  happen directly on the server.
- **Work on a file:** drag it out, edit it, drag it back.
- **Never lose work:** if someone changed the file in the meantime, SSH-Cloud
  asks before it overwrites. No locks, no conflicts hidden from you.
- **Nothing to install on the server:** plain OpenSSH (SFTP) is all it needs.
- **Feels like home:** a Windows 11 style window with tabs for several
  connections and a details pane.

## Get started

Install the MSI from the [releases](https://github.com/mevdschee/ssh-cloud/releases),
run SSH-Cloud from the Start menu, enter your server, username and folder, and
you have a cloud. Log in with your SSH agent, Pageant, a key file or a password.
Use the update button to get new versions.

## Build

```
go install github.com/tc-hib/go-winres@latest
./run.sh                 # build ssh-cloud.exe and run it with Wine
./release.sh [minor]     # build the MSI and publish a GitHub release
```

## License

MIT. Icons from [Fluent UI System Icons](https://github.com/microsoft/fluentui-system-icons) (MIT).
