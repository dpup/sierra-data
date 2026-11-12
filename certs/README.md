# certs

`rds-global-bundle.pem` is Amazon's RDS global certificate bundle
(<https://truststore.pki.rds.amazonaws.com/global/global-bundle.pem>), the root
CAs that sign every regional RDS server certificate. The Dockerfile adds it to
the Grid image's system trust store so `PF__GRID__DATABASE__URL` can use
`sslmode=verify-full` with no `sslrootcert` parameter; `gridctl` in the same
image verifies the same way. Vendored so an image build needs no network and
the deploy's content key covers it. Refresh it from the URL above when AWS
publishes new CAs (the file is public; nothing in it is a secret).
