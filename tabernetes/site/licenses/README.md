# Licenses of text that Tabernetes reproduces

The built-in images of the tab node (`k8s/tabernetes/tabnode_images*.go`)
contain files of the images they stand in for. `site/build.mjs` puts these
license files into `THIRD_PARTY_LICENSES.txt` of the page:

- `nginx-LICENSE`: nginx 1.29.3, <https://github.com/nginx/nginx>
  (BSD-2-Clause): the default pages, `nginx.conf`, `conf.d/default.conf` and
  `mime.types` of the nginx image.
- `docker-nginx-LICENSE`: the official nginx image,
  <https://github.com/nginx/docker-nginx> (BSD-2-Clause).
- `httpd-NOTICE`: the NOTICE of the Apache HTTP Server,
  <https://github.com/apache/httpd> (Apache License 2.0), for the shortened
  `httpd.conf` of the httpd image.
