## Spike B - six tiles, 3 min sampled after 30 s warm-up (CPU is the sum over ALL processes of the test browser; GPU is the test browser's own engine use)

### (ii) six 640x360 sub-stream stand-ins

| browser | HW decode | CPU % of 1 core mean / max | busiest thread % mean / max | 1070 Ti VideoDecode % | 1070 Ti 3D % | working set MB | min fps | decoder-dropped frames (6 tiles) | render-dropped (6 tiles) | freezes (total s) | verdict |
|---|---|---|---|---|---|---|---|---|---|---|---|
| chrome | on (ExternalDecoder (D3D11VideoDecoder) (power-efficient)) | 53.4 / 129.7 | 19.9 / 45.8 | 6.6 | 0.9 | 742 | 20.0 | 2 (0.01%) | 234 (1.1%) | 0 (0.0 s) | smooth |
| chrome | off (FFmpeg) | 59.3 / 206.2 | 20.6 / 108 | 0.0 | 2.7 | 697 | 20.0 | 6 (0.03%) | 67 (0.3%) | 0 (0.0 s) | smooth |
| edge | on (ExternalDecoder (D3D11VideoDecoder) (power-efficient)) | 54.7 / 143.4 | 19.8 / 48.3 | 6.7 | 1.1 | 617 | 20.0 | 0 (0.00%) | 2358 (10.8%) | 47 (10.7 s) | marginal |
| edge | off (FFmpeg) | 52.9 / 150.5 | 16.4 / 36.1 | 0.0 | 2.6 | 550 | 20.0 | 0 (0.00%) | 69 (0.3%) | 2 (0.5 s) | smooth |
| mpv (dashboard flags) | software, hwdec=no | 38.3 / 92.6 | 4.7 / 13.7 | 0.0 | 8.5 | 443 | - | 0 (VO+decoder) | - | - | smooth |

### (i) six 2560x1440 mains

| browser | HW decode | CPU % of 1 core mean / max | busiest thread % mean / max | 1070 Ti VideoDecode % | 1070 Ti 3D % | working set MB | min fps | decoder-dropped frames (6 tiles) | render-dropped (6 tiles) | freezes (total s) | verdict |
|---|---|---|---|---|---|---|---|---|---|---|---|
| chrome | on (ExternalDecoder (D3D11VideoDecoder) (power-efficient)) | 64.7 / 143.5 | 25.3 / 54.4 | 41.9 | 0.6 | 772 | 20.0 | 15 (0.07%) | 229 (1.0%) | 2 (0.6 s) | smooth |
| chrome | off (FFmpeg) | 150.1 / 255.8 | 34.5 / 58.5 | 0.0 | 21.9 | 1542 | 16.5 | 3566 (14.71%) | 1611 (7.8%) | 350 (142.2 s) | unusable |
| edge | on (ExternalDecoder (D3D11VideoDecoder) (power-efficient)) | 67.4 / 178.1 | 26.9 / 72.7 | 38.9 | 0.6 | 654 | 20.0 | 3 (0.01%) | 281 (1.3%) | 1 (0.2 s) | smooth |
| edge | off (FFmpeg) | 185.6 / 525.2 | 54.6 / 161 | 0.0 | 24.0 | 1090 | 18.6 | 648 (2.78%) | 1026 (4.5%) | 102 (38.8 s) | unusable |
| mpv (dashboard flags) | software, hwdec=no | 140.6 / 378.3 | 18.1 / 57.2 | 0.0 | 21.7 | 660 | - | 0 (VO+decoder) | - | - | smooth |

### (iii) six 1280x720

| browser | HW decode | CPU % of 1 core mean / max | busiest thread % mean / max | 1070 Ti VideoDecode % | 1070 Ti 3D % | working set MB | min fps | decoder-dropped frames (6 tiles) | render-dropped (6 tiles) | freezes (total s) | verdict |
|---|---|---|---|---|---|---|---|---|---|---|---|
| chrome | on (ExternalDecoder (D3D11VideoDecoder) (power-efficient)) | 60.5 / 139.2 | 23.1 / 48 | 19.9 | 2.0 | 738 | 20.0 | 0 (0.00%) | 258 (1.1%) | 1 (0.2 s) | smooth |
| chrome | off (FFmpeg) | 90.9 / 175.3 | 26.9 / 52.6 | 0.0 | 6.6 | 1762 | 19.9 | 25 (0.11%) | 145 (0.7%) | 5 (1.2 s) | marginal |
| edge | on (ExternalDecoder (D3D11VideoDecoder) (power-efficient)) | 60.2 / 121 | 23.9 / 42.7 | 19.5 | 1.6 | 621 | 20.0 | 8 (0.04%) | 160 (0.7%) | 0 (0.0 s) | smooth |
| edge | off (FFmpeg) | 92.4 / 198.9 | 26.7 / 63.1 | 0.0 | 6.1 | 633 | 20.0 | 3 (0.01%) | 19 (0.1%) | 0 (0.0 s) | smooth |

### (iv) 5 x 640x360 + 1 x 2560x1440 focused (3x3, main spans 2x2)

| browser | HW decode | CPU % of 1 core mean / max | busiest thread % mean / max | 1070 Ti VideoDecode % | 1070 Ti 3D % | working set MB | min fps | decoder-dropped frames (6 tiles) | render-dropped (6 tiles) | freezes (total s) | verdict |
|---|---|---|---|---|---|---|---|---|---|---|---|
| chrome | on (ExternalDecoder (D3D11VideoDecoder) (power-efficient)) | 54.1 / 112.8 | 20.4 / 47.5 | 17.5 | 1.8 | 733 | 20.0 | 5 (0.02%) | 151 (0.7%) | 0 (0.0 s) | smooth |
| chrome | off (FFmpeg) | 74.9 / 225.1 | 18.9 / 42.7 | 0.0 | 6.1 | 744 | 20.0 | 6 (0.03%) | 78 (0.3%) | 0 (0.0 s) | smooth |
| edge | on (ExternalDecoder (D3D11VideoDecoder) (power-efficient)) | 54.7 / 96.2 | 20.1 / 34.5 | 17.5 | 1.7 | 614 | 20.0 | 4 (0.02%) | 158 (0.7%) | 0 (0.0 s) | smooth |
| edge | off (FFmpeg) | 72.9 / 144.1 | 15.8 / 34.7 | 0.0 | 6.3 | 639 | 19.9 | 12 (0.05%) | 64 (0.3%) | 0 (0.0 s) | smooth |

Noise floor (the sampler process alone, no browser; rig = MediaMTX + publishers running): sampler CPU 45.9% of a core, rig CPU 6.3%, WHOLE-MACHINE CPU 74.9%.

## Whole-machine CPU during each cell (%, 4 logical CPUs = 100)

| cell | machine CPU % | rig CPU % of 1 core |
|---|---|---|
| focus-chrome-hwoff | 92.7 | 8.5 |
| focus-chrome-hwon | 87.7 | 8.7 |
| focus-edge-hwoff | 93.1 | 8.6 |
| focus-edge-hwon | 89.0 | 8.9 |
| hd6-chrome-hwoff | 96.6 | 10.5 |
| hd6-chrome-hwon | 89.8 | 10.5 |
| hd6-edge-hwoff | 93.3 | 11.7 |
| hd6-edge-hwon | 86.8 | 10.9 |
| idle | 74.9 | 6.3 |
| main6-chrome-hwoff | 100.0 | 10.6 |
| main6-chrome-hwon | 89.0 | 13.2 |
| main6-edge-hwoff | 100.0 | 11.7 |
| main6-edge-hwon | 87.4 | 13.6 |
| main6-mpv | 100.0 | 12.8 |
| sub6-chrome-hwoff | 85.8 | 8.0 |
| sub6-chrome-hwon | 86.2 | 8.3 |
| sub6-edge-hwoff | 87.3 | 7.4 |
| sub6-edge-hwon | 85.6 | 8.3 |
| sub6-mpv | 83.0 | 6.7 |

## Hidden-tab runs (6 x 1440p, HW decode on): window minimized after 60 s

| browser | phase | CPU % mean | 1070 Ti VideoDecode % | decoded fps per tile | page visibilityState |
|---|---|---|---|---|---|
| chrome | visible | 64.5 | 31.5 | 19.9-20.0 | visible |
| chrome | hidden | 20.6 | 14.5 | 5.2-6.0 | hidden |
| edge | visible | 67.1 | 40.2 | 19.1-19.6 | visible |
| edge | hidden | 23.3 | 17.9 | 4.9-9.9 | hidden |

## Spike A - one 2560x1440 tile

| run | browser | decoder implementation | CPU % mean / max | busiest thread % | 1070 Ti VideoDecode % | 1070 Ti 3D % | WS MB | fps | dropped / freezes | jitter-buffer ms | decode ms/frame | time to first frame ms (incl. ICE gather) | note |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| A-chrome-clean | chrome | ExternalDecoder (D3D11VideoDecoder) | 19.2 / 115.1 | 4.6 | 12.2 | 0.7 | 628 | 20.0 | 0 / 0 | 295.1 | 1.74 | 2119 |  |
| A-chrome-probe | chrome | ExternalDecoder (D3D11VideoDecoder) | 75.6 / 125.7 | 34.7 | 10.1 | 5.5 | 822 | 20.0 | 0 / 0 | 205.6 | 3.47 | 1182 | canvas readback + unmute: CPU/GPU inflated by the probe; barcode latency median 0.246 s (p10 0.174, p90 0.272); receive-to-callback median 243.2 ms; audio energy +0.09573, level 0.038087099826044496 |
| A-edge-clean | edge | ExternalDecoder (D3D11VideoDecoder) | 21.4 / 76.8 | 4.9 | 11.0 | 0.5 | 518 | 20.0 | 0 / 0 | 137.0 | 1.46 | 2279 |  |
| A-edge-probe | edge | ExternalDecoder (D3D11VideoDecoder) | 82.8 / 130.9 | 41.0 | 13.1 | 7.1 | 611 | 20.0 | 0 / 0 | 174.2 | 4.84 | 1532 | canvas readback + unmute: CPU/GPU inflated by the probe; barcode latency median 0.198 s (p10 0.101, p90 0.265); receive-to-callback median 211.5 ms; audio energy +0.08994, level 0.038087099826044496 |
| A-firefox-clean | firefox | None | 41.3 / 152.1 | 12.7 | 0.0 | 0.0 | 779 | 0.0 | 0 / 0 | None | None | None | never connected (ICE failed) |

## Decoder implementation runs (single tile, capture on so getStats exposes it)

| run | implementation | power-efficient | 1070 Ti VideoDecode % | HD 530 VideoDecode % |
|---|---|---|---|---|
| I2-chrome-hwoff-m1 | FFmpeg | False | 0.0 | 0.0 |
| I2-chrome-hwoff-s1 | FFmpeg | False | 0.0 | 0.0 |
| I2-chrome-hwon-m1 | ExternalDecoder (D3D11VideoDecoder) | True | 12.8 | 0.0 |
| I2-chrome-hwon-s1 | ExternalDecoder (D3D11VideoDecoder) | True | 0.5 | 0.0 |
| I2-edge-hwoff-m1 | FFmpeg | False | 0.0 | 0.0 |
| I2-edge-hwoff-s1 | FFmpeg | False | 0.0 | 0.0 |
| I2-edge-hwon-m1 | ExternalDecoder (D3D11VideoDecoder) | True | 12.7 | 0.0 |
| I2-edge-hwon-s1 | ExternalDecoder (D3D11VideoDecoder) | True | 0.5 | 0.0 |
