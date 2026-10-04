package main

import (
	"html"
	"net"
	"net/http"
	"strings"
)

// A decoy answers every web path like a freshly installed web server: the
// front page is its stock welcome page and everything else is its stock
// error page. Only /api/v1 and live setup links get past it.
type decoyPage struct {
	server string                                 // Server header, "" for none
	index  func(host string) string               // the front page
	error  func(code int, r *http.Request) string // body for 404 and 405
}

var decoyPages = map[string]decoyPage{
	"nginx":  {server: nginxServer, index: func(string) string { return nginxIndex }, error: nginxError},
	"apache": {server: apacheServer, index: func(string) string { return apacheIndex }, error: apacheError},
	"soon":   {index: soonIndex, error: soonError},
}

// serveDecoy writes the decoy's answer for r. It drops the headers the web
// interface adds, since a stock server sends none of them.
func serveDecoy(w http.ResponseWriter, r *http.Request, name string) {
	d, ok := decoyPages[name]
	if !ok {
		d = decoyPages["nginx"]
	}
	h := w.Header()
	for _, k := range []string{"Content-Security-Policy", "X-Content-Type-Options", "Referrer-Policy", "X-Frame-Options", "Strict-Transport-Security", "Cache-Control"} {
		h.Del(k)
	}
	if d.server != "" {
		h.Set("Server", d.server)
	}
	h.Set("Content-Type", "text/html")
	code, body := http.StatusOK, ""
	switch {
	case r.Method != http.MethodGet && r.Method != http.MethodHead:
		code, body = http.StatusMethodNotAllowed, d.error(http.StatusMethodNotAllowed, r)
	case r.URL.Path == "/" || r.URL.Path == "/index.html":
		body = d.index(hostOnly(r.Host))
	default:
		code, body = http.StatusNotFound, d.error(http.StatusNotFound, r)
	}
	w.WriteHeader(code)
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte(body))
	}
}

func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}

func hostPort(r *http.Request) string {
	if _, p, err := net.SplitHostPort(r.Host); err == nil {
		return p
	}
	if r.TLS != nil {
		return "443"
	}
	return "80"
}

const nginxServer = "nginx/1.24.0 (Ubuntu)"

const nginxIndex = `<!DOCTYPE html>
<html>
<head>
<title>Welcome to nginx!</title>
<style>
html { color-scheme: light dark; }
body { width: 35em; margin: 0 auto;
font-family: Tahoma, Verdana, Arial, sans-serif; }
</style>
</head>
<body>
<h1>Welcome to nginx!</h1>
<p>If you see this page, the nginx web server is successfully installed and
working. Further configuration is required.</p>

<p>For online documentation and support please refer to
<a href="http://nginx.org/">nginx.org</a>.<br/>
Commercial support is available at
<a href="http://nginx.com/">nginx.com</a>.</p>

<p><em>Thank you for using nginx.</em></p>
</body>
</html>
`

func nginxError(code int, _ *http.Request) string {
	status := statusLine(code)
	return "<html>\r\n<head><title>" + status + "</title></head>\r\n<body>\r\n<center><h1>" + status +
		"</h1></center>\r\n<hr><center>" + nginxServer + "</center>\r\n</body>\r\n</html>\r\n"
}

const apacheServer = "Apache/2.4.58 (Ubuntu)"

func apacheError(code int, r *http.Request) string {
	msg := "<p>The requested URL was not found on this server.</p>"
	if code == http.StatusMethodNotAllowed {
		msg = "<p>The requested method " + html.EscapeString(r.Method) + " is not allowed for this URL.</p>"
	}
	return "<!DOCTYPE HTML PUBLIC \"-//IETF//DTD HTML 2.0//EN\">\n<html><head>\n<title>" + statusLine(code) +
		"</title>\n</head><body>\n<h1>" + http.StatusText(code) + "</h1>\n" + msg + "\n<hr>\n<address>" + apacheServer +
		" Server at " + html.EscapeString(hostOnly(r.Host)) + " Port " + hostPort(r) + "</address>\n</body></html>\n"
}

func soonIndex(host string) string {
	return strings.ReplaceAll(soonTemplate, "{{host}}", html.EscapeString(host))
}

func soonError(code int, _ *http.Request) string {
	status := statusLine(code)
	return "<!DOCTYPE html>\n<html>\n<head><title>" + status + "</title></head>\n<body>\n<h1>" + status + "</h1>\n</body>\n</html>\n"
}

func statusLine(code int) string {
	if code == http.StatusMethodNotAllowed {
		return "405 Not Allowed" // nginx's wording, also fine for the others
	}
	return "404 Not Found"
}

const soonTemplate = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Coming soon</title>
<style>
html, body { height: 100%; margin: 0; }
body { display: flex; align-items: center; justify-content: center; background: #f7f7f7; color: #444;
font-family: Helvetica, Arial, sans-serif; text-align: center; }
h1 { font-size: 28px; font-weight: 600; color: #222; margin: 0 0 10px; }
p { margin: 0 0 6px; }
.host { font-size: 13px; color: #888; margin-top: 18px; }
</style>
</head>
<body>
<main>
<h1>Coming soon</h1>
<p>This site is under construction.</p>
<p class="host">{{host}}</p>
</main>
</body>
</html>
`

const apacheIndex = `<!DOCTYPE html PUBLIC "-//W3C//DTD XHTML 1.0 Transitional//EN" "http://www.w3.org/TR/xhtml1/DTD/xhtml1-transitional.dtd">
<html xmlns="http://www.w3.org/1999/xhtml">
  <head>
    <meta http-equiv="Content-Type" content="text/html; charset=UTF-8" />
    <title>Apache2 Ubuntu Default Page: It works</title>
    <style type="text/css" media="screen">
  * {
    margin: 0px 0px 0px 0px;
    padding: 0px 0px 0px 0px;
  }

  body, html {
    padding: 3px 3px 3px 3px;

    background-color: #D8DBE2;

    font-family: Ubuntu, Verdana, sans-serif;
    font-size: 11pt;
    text-align: center;
  }

  div.main_page {
    position: relative;
    display: table;

    width: 800px;

    margin-bottom: 3px;
    margin-left: auto;
    margin-right: auto;
    padding: 0px 0px 0px 0px;

    border-width: 2px;
    border-color: #212738;
    border-style: solid;

    background-color: #FFFFFF;

    text-align: center;
  }

  div.page_header {
    height: 180px;
    width: 100%;

    background-color: #F5F6F7;
  }

  div.page_header span {
    margin: 15px 0px 0px 50px;

    font-size: 180%;
    font-weight: bold;
  }

  div.page_header img {
    margin: 3px 0px 0px 40px;

    border: 0px 0px 0px;
  }

  div.banner {
    padding: 9px 6px 9px 6px;
    background-color: #E9510E;
    color: #FFFFFF;
    font-weight: bold;
    font-size: 112%;
    text-align: center;
    position: absolute;
    left: 40%;
    bottom: 30px;
    width: 20%;
  }

  div.table_of_contents {
    clear: left;

    min-width: 200px;

    margin: 3px 3px 3px 3px;

    background-color: #FFFFFF;

    text-align: left;
  }

  div.table_of_contents_item {
    clear: left;

    width: 100%;

    margin: 4px 0px 0px 0px;

    background-color: #FFFFFF;

    color: #000000;
    text-align: left;
  }

  div.table_of_contents_item a {
    margin: 6px 0px 0px 6px;
  }

  div.content_section {
    margin: 3px 3px 3px 3px;

    background-color: #FFFFFF;

    text-align: left;
  }

  div.content_section_text {
    padding: 4px 8px 4px 8px;

    color: #000000;
    font-size: 100%;
  }

  div.content_section_text pre {
    margin: 8px 0px 8px 0px;
    padding: 8px 8px 8px 8px;

    border-width: 1px;
    border-style: dotted;
    border-color: #000000;

    background-color: #F5F6F7;

    font-style: italic;
  }

  div.content_section_text p {
    margin-bottom: 6px;
  }

  div.content_section_text ul, div.content_section_text li {
    padding: 4px 8px 4px 16px;
  }

  div.section_header {
    padding: 3px 6px 3px 6px;

    background-color: #8E9CB2;

    color: #FFFFFF;
    font-weight: bold;
    font-size: 112%;
    text-align: center;
  }

  div.section_header_red {
    background-color: #CD214F;
  }

  div.section_header_grey {
    background-color: #9F9386;
  }

  .floating_element {
    position: relative;
    float: left;
  }

  div.table_of_contents_item a,
  div.content_section_text a {
    text-decoration: none;
    font-weight: bold;
  }

  div.table_of_contents_item a:link,
  div.table_of_contents_item a:visited,
  div.table_of_contents_item a:active {
    color: #000000;
  }

  div.table_of_contents_item a:hover {
    background-color: #000000;

    color: #FFFFFF;
  }

  div.content_section_text a:link,
  div.content_section_text a:visited,
   div.content_section_text a:active {
    background-color: #DCDFE6;

    color: #000000;
  }

  div.content_section_text a:hover {
    background-color: #000000;

    color: #DCDFE6;
  }

  div.validator {
  }
    </style>
  </head>
  <body>
    <div class="main_page">
      <div class="page_header floating_element">
        <span class="floating_element">
          Apache2 Default Page
        </span>
      </div>
<!--      <div class="table_of_contents floating_element">
        <div class="section_header section_header_grey">
          TABLE OF CONTENTS
        </div>
        <div class="table_of_contents_item floating_element">
          <a href="#about">About</a>
        </div>
        <div class="table_of_contents_item floating_element">
          <a href="#changes">Changes</a>
        </div>
        <div class="table_of_contents_item floating_element">
          <a href="#scope">Scope</a>
        </div>
        <div class="table_of_contents_item floating_element">
          <a href="#files">Config files</a>
        </div>
      </div>
-->
      <div class="content_section floating_element">


        <div class="section_header section_header_red">
          <div id="about"></div>
          It works!
        </div>
        <div class="content_section_text">
          <p>
                This is the default welcome page used to test the correct
                operation of the Apache2 server after installation on Ubuntu systems.
                It is based on the equivalent page on Debian, from which the Ubuntu Apache
                packaging is derived.
                If you can read this page, it means that the Apache HTTP server installed at
                this site is working properly. You should <b>replace this file</b> (located at
                <tt>/var/www/html/index.html</tt>) before continuing to operate your HTTP server.
          </p>


          <p>
                If you are a normal user of this web site and don't know what this page is
                about, this probably means that the site is currently unavailable due to
                maintenance.
                If the problem persists, please contact the site's administrator.
          </p>

        </div>
        <div class="section_header">
          <div id="changes"></div>
                Configuration Overview
        </div>
        <div class="content_section_text">
          <p>
                Ubuntu's Apache2 default configuration is different from the
                upstream default configuration, and split into several files optimized for
                interaction with Ubuntu tools. The configuration system is
                <b>fully documented in
                /usr/share/doc/apache2/README.Debian.gz</b>. Refer to this for the full
                documentation. Documentation for the web server itself can be
                found by accessing the <a href="/manual">manual</a> if the <tt>apache2-doc</tt>
                package was installed on this server.
          </p>
          <p>
                The configuration layout for an Apache2 web server installation on Ubuntu systems is as follows:
          </p>
          <pre>
/etc/apache2/
|-- apache2.conf
|       ` + "`" + `--  ports.conf
|-- mods-enabled
|       |-- *.load
|       ` + "`" + `-- *.conf
|-- conf-enabled
|       ` + "`" + `-- *.conf
|-- sites-enabled
|       ` + "`" + `-- *.conf
          </pre>
          <ul>
                        <li>
                           <tt>apache2.conf</tt> is the main configuration
                           file. It puts the pieces together by including all remaining configuration
                           files when starting up the web server.
                        </li>

                        <li>
                           <tt>ports.conf</tt> is always included from the
                           main configuration file. It is used to determine the listening ports for
                           incoming connections, and this file can be customized anytime.
                        </li>

                        <li>
                           Configuration files in the <tt>mods-enabled/</tt>,
                           <tt>conf-enabled/</tt> and <tt>sites-enabled/</tt> directories contain
                           particular configuration snippets which manage modules, global configuration
                           fragments, or virtual host configurations, respectively.
                        </li>

                        <li>
                           They are activated by symlinking available
                           configuration files from their respective
                           *-available/ counterparts. These should be managed
                           by using our helpers
                           <tt>
                                a2enmod,
                                a2dismod,
                           </tt>
                           <tt>
                                a2ensite,
                                a2dissite,
                            </tt>
                                and
                           <tt>
                                a2enconf,
                                a2disconf
                           </tt>. See their respective man pages for detailed information.
                        </li>

                        <li>
                           The binary is called apache2 and is managed using systemd, so to
                           start/stop the service use <tt>systemctl start apache2</tt> and
                           <tt>systemctl stop apache2</tt>, and use <tt>systemctl status apache2</tt>
                           and <tt>journalctl -u apache2</tt> to check status.  <tt>system</tt>
                           and <tt>apache2ctl</tt> can also be used for service management if
                           desired.
                           <b>Calling <tt>/usr/bin/apache2</tt> directly will not work</b> with the
                           default configuration.
                        </li>
          </ul>
        </div>

        <div class="section_header">
            <div id="docroot"></div>
                Document Roots
        </div>

        <div class="content_section_text">
            <p>
                By default, Ubuntu does not allow access through the web browser to
                <em>any</em> file outside of those located in <tt>/var/www</tt>,
                <a href="http://httpd.apache.org/docs/2.4/mod/mod_userdir.html" rel="nofollow">public_html</a>
                directories (when enabled) and <tt>/usr/share</tt> (for web
                applications). If your site is using a web document root
                located elsewhere (such as in <tt>/srv</tt>) you may need to whitelist your
                document root directory in <tt>/etc/apache2/apache2.conf</tt>.
            </p>
            <p>
                The default Ubuntu document root is <tt>/var/www/html</tt>. You
                can make your own virtual hosts under /var/www.
            </p>
        </div>

        <div class="section_header">
          <div id="bugs"></div>
                Reporting Problems
        </div>
        <div class="content_section_text">
          <p>
                Please use the <tt>ubuntu-bug</tt> tool to report bugs in the
                Apache2 package with Ubuntu. However, check <a
                href="https://bugs.launchpad.net/ubuntu/+source/apache2"
                rel="nofollow">existing bug reports</a> before reporting a new bug.
          </p>
          <p>
                Please report bugs specific to modules (such as PHP and others)
                to their respective packages, not to the web server itself.
          </p>
        </div>




      </div>
    </div>
    <div class="validator">
    </div>
  </body>
</html>
`
