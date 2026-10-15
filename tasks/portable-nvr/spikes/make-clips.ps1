# Encodes the three 30 s looping clips ONCE (libx264 Main, no B-frames, GOP 40, yuv420p, G.711 A-law mono 8 kHz).
# Burned in: frame counter + clip time as text, and a 10-bit frame-number barcode along the top edge (cells 1..10,
# cell 0 is always white), which the test page can decode in-browser for the latency probe.
. (Join-Path (Split-Path -Parent $MyInvocation.MyCommand.Path) 'common.ps1')
$bitrate = @{ m='1500k'; h='1000k'; s='500k' }
$total = $Fps * $ClipSeconds
foreach($k in 'm','h','s'){
  $W = $Sets[$k].W; $H = $Sets[$k].H
  $out = Join-Path $ClipDir ("clip-{0}x{1}.mkv" -f $W,$H)
  $fs = [int]($H / 28)
  $f = New-Object System.Collections.Generic.List[string]
  $f.Add("noise=alls=8:allf=t")
  $f.Add("drawbox=x=0:y=0:w=iw:h=ih/18:color=black:t=fill")
  $f.Add("drawbox=x=0:y=0:w=iw/16:h=ih/18:color=white:t=fill")
  for($b=0;$b -lt 10;$b++){ $f.Add(("drawbox=x=iw/16*{0}:y=0:w=iw/16:h=ih/18:color=white:t=fill:enable='bitand(n,{1})'" -f ($b+1),(1 -shl $b))) }
  $f.Add(("drawtext=fontfile='C\:/Windows/Fonts/consola.ttf':fontsize={0}:fontcolor=white:box=1:boxcolor=black@0.6:x=20:y=h/18+20:text='{1}x{2}  frame %{{frame_num}}/{3}  t=%{{pts\:flt}}s'" -f $fs,$W,$H,$total))
  $fp = Join-Path $Rt "filter-$k.txt"
  ($f -join ",`n") | Set-Content $fp -Encoding ascii
  $a = @('-y','-hide_banner','-loglevel','warning',
    '-f','lavfi','-i',("testsrc2=size={0}x{1}:rate={2}:duration={3}" -f $W,$H,$Fps,$ClipSeconds),
    '-f','lavfi','-i',("sine=frequency=440:sample_rate=8000:duration={0}" -f $ClipSeconds),
    '-/filter:v',$fp,'-af','volume=0.3',
    '-c:v','libx264','-preset','medium','-profile:v','main','-bf','0','-g','40','-sc_threshold','0',
    '-b:v',$bitrate[$k],'-maxrate',$bitrate[$k],'-bufsize',('{0}k' -f (2*[int]($bitrate[$k].TrimEnd('k')))),'-pix_fmt','yuv420p',
    '-c:a','pcm_alaw','-ar','8000','-ac','1','-t',"$ClipSeconds",$out)
  $sw = [Diagnostics.Stopwatch]::StartNew()
  & $Ffmpeg @a
  if($LASTEXITCODE -ne 0){ throw "ffmpeg failed for $k" }
  '{0}: encoded in {1:n0} s' -f $out,$sw.Elapsed.TotalSeconds
  & $Ffprobe -v error -show_entries stream=codec_name,profile,width,height,r_frame_rate,bit_rate,has_b_frames,duration -of compact $out
}
