# Bundles written by v1.6.0

Azud 2 reads stored v1 releases for all of 2.x, so rollback keeps working after an
upgrade. These bundles were written by `release.Build` and `release.Encrypt` at the
`v1.6.0` tag, and `TestV1BundlesStayReadable` reads them through a registry. Never
regenerate them: their value is that today's code did not write them.

`full/identity.txt` is a throwaway age key made for this fixture. It decrypts only
`full/secrets.age`, which holds a made-up value.
