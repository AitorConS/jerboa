# MongoDB on Jerboa — persistent data with volumes

`mongod` initialises an empty `--dbpath` on first start, so MongoDB needs no
separate seed step: just attach a volume at `/data/db`.

## 1. Create a volume

```sh
jerboa volume create mongodata --size 800M
```

## 2. Run with the volume mounted at the data path

```sh
jerboa build . -t mongodb
jerboa run mongodb -v mongodata:/data/db --network mynet --port 27017:27017
```

Data written by `mongod` lands in the `mongodata` volume and survives VM
restarts. The empty `/data/db` directory baked into the image (see the
`disk_size`/empty-dir handling in `unikernel.toml`) is only the mount point —
the volume's contents are what persist.

To start fresh, `jerboa volume rm mongodata`.

See `../postgresql/README.md` for the general volume-seeding model (TFS label
matching, QEMU `opt/uni/mounts` fw_cfg vs. Firecracker boot args).
