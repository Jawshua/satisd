
satisd
==========

satisd is a very tiny daemon for [satis](https://github.com/composer/satis), a static Composer repository generator. Its purpose is to dynamically add packages to a private Composer repository from GitHub or GitLab webhook events. It also serves out the generated repository for good measure.

Be aware that satisd is designed to be used in a trusted environment. There is no authentication of any kind.


Usage
------------

Follow the [satis documentation](https://getcomposer.org/doc/articles/handling-private-packages-with-satis.md) to install satis and create a repo config file. In the following examples, we've saved our satis config file to `/opt/repo/repo.json` and our satis output repository folder will be `/opt/repo/build`,



**Run natively:**

- Compile `satisd.go` with `go install`, or grab a binary from the [releases](https://github.com/Jawshua/satisd/releases) page.


- Run `satisd -satis /opt/satis/bin/satis -config /opt/repo/repo.json -repo /opt/repo/build` 
  - Optionally specify the `-listen` flag to change the HTTP listen address
- satisd will then listen for HTTP requests and trigger a satis build on calls to `/register` or `/generate`




**Run with docker:**

`docker run --name satisd -v /opt/repo/config.json:/satisd/config.json -v /opt/repo/build:/satisd/repo jawshua/satisd`

**Run with docker, bind to host port 8080:**

`docker run --name satisd -p 8080:80 -v /opt/repo/repo.json:/satisd/config.json -v /opt/repo/build:/satisd/repo jawshua/satisd`

Endpoints
---------------------
The `/register` endpoint registers a package and then does a satis build. It takes 4 query params:
- *package* - the name of the composer package
- *version* - the package version _(default: *)_
- *repo* - the repository that the package is located in
- *repoType* - the type of repository

The `/generate` endpoint will instantly trigger a satis build. It does not take any query params.

The `/config.json` endpoint will show the current repository configuration file.

All other URLs will attempt to serve a file from the generated repository.