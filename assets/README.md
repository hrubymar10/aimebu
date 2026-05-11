# Icon Assets

`assets/aimebu.png` is the hand-authored source-of-truth icon for aimebu.
Do not edit generated variants by hand.

Regenerate the frontend variants after any change to `assets/aimebu.png`:

```bash
magick assets/aimebu.png -resize 32x32 -background none -gravity center -extent 32x32 frontend/icons/aimebu-32.png
magick assets/aimebu.png -resize 180x180 -background none -gravity center -extent 180x180 frontend/icons/aimebu-180.png
magick assets/aimebu.png -resize 192x192 -background none -gravity center -extent 192x192 frontend/icons/aimebu-192.png
magick assets/aimebu.png -resize 512x512 -background none -gravity center -extent 512x512 frontend/icons/aimebu-512.png
```

Theme/browser chrome color:

```bash
# The rounded corner is transparent at 10,10, so sample the first opaque
# pixel inside the top-left frame instead.
magick "assets/aimebu.png[1x1+74+74]" txt:-
```

The current sampled color is `#08223d`; keep `frontend/index.html` and
`frontend/manifest.webmanifest` in sync with the sampled value whenever the
source asset changes.
