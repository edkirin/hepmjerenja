package main

// Embed the IANA time zone database into the binary.
//
// Every date bucket in this application is expressed in Europe/Zagreb local time,
// and hep_client.go panics during init if that zone cannot be loaded. Without this
// import the zone comes from the operating system, which is fine on a typical Linux
// or macOS install but not elsewhere: Windows ships no IANA database at all, and a
// minimal Linux image does not either — deleting /usr/share/zoneinfo from the
// runtime image is enough to make the binary panic on startup.
//
// The embedded copy costs roughly 450 KB and makes the released binaries
// self-contained on every platform. A zone database found on the system still
// wins; this is only the fallback.
import _ "time/tzdata"
