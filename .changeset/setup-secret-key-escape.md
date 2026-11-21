---
"ftw": patch
---

Setup and Settings no longer let a quote in a value break the page. The
setup wizard builds each driver secret field as page elements instead of
HTML text, and Settings now escapes quotes. Before, a value with a quote,
such as an MQTT username or a charger's reported features, was cut short
in its field and could add markup to the page.
