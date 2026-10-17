<#
    golden-time.ps1 - the daylight-saving table of the PowerShell product: how
    Format-CamTimestamp writes, and how Get-CamDateTimeOrNull, Test-CamIntervalDue
    and Get-CamSupervisorState (heartbeat age) read, instants around the two
    transitions of the zone the goldens use. Dot-source holocron-lib.ps1 first.

    The zone is Windows 'GTB Standard Time' (IANA Europe/Bucharest):
      2026-10-25 04:00 -> 03:00 local (01:00:00 UTC), clocks go back;
      2027-03-28 03:00 -> 04:00 local (01:00:00 UTC), clocks go forward.
    The machine clock and zone are never changed: every instant is constructed
    in UTC and converted by the zone's own rules, exactly as the dst check of
    tests\cam.tests.ps1 does.
#>

function New-HolocronTimeGoldens {
    param([Parameter(Mandatory = $true)][string]$OutDir)

    $script:HolocronTimeNow = [DateTime]::UtcNow
    function Get-CamWallClock { return $script:HolocronTimeNow }

    $utcText = { param([DateTime]$t) $t.ToString('yyyy-MM-ddTHH:mm:ss.fffffffZ') }
    $wall = { param([DateTime]$t) $t.ToLocalTime().ToString('yyyy-MM-dd HH:mm:ss') }
    $offsetText = {
        param([DateTime]$t)
        $o = [TimeZoneInfo]::Local.GetUtcOffset($t)
        $sign = $(if ($o -lt [TimeSpan]::Zero) { '-' } else { '+' })
        return ('{0}{1:00}{2:00}' -f $sign, [math]::Abs($o.Hours), [math]::Abs($o.Minutes))
    }

    $start = [DateTime]::new(2026, 10, 1, 0, 0, 0, [DateTimeKind]::Utc)
    $transitions = @(Get-CamTestDstTransitions -FromUtc $start)
    $expected = @('2026-10-25T01:00:00Z', '2027-03-28T01:00:00Z')
    if ($transitions.Count -ne 2 -or $transitions[0].AtUtc.ToString('yyyy-MM-ddTHH:mm:ssZ') -ne $expected[0] -or $transitions[1].AtUtc.ToString('yyyy-MM-ddTHH:mm:ssZ') -ne $expected[1]) {
        throw 'the zone does not have the expected 2026-10-25 / 2027-03-28 transitions'
    }

    $config = Get-CamConfigDefault
    $config.StatusIntervalSeconds = 5
    $limit = [math]::Max(15, 3 * [double]$config.StatusIntervalSeconds)

    $blocks = @()
    foreach ($tr in $transitions) {
        $change = $tr.AtUtc
        $instants = @()
        foreach ($d in @(-3600, -1800, -600, -60, -1, 0, 1, 60, 600, 1800, 3600)) {
            $t = $change.AddSeconds($d)
            $formatted = Format-CamTimestamp -Value $t
            $back = Get-CamDateTimeOrNull -Value $formatted
            $local = $t.ToLocalTime()
            $instants += [ordered]@{
                secondsFromChange = $d
                utc               = (& $utcText $t)
                formatted         = $formatted
                parsedBackUtc     = (& $utcText $back)
                localWall         = (& $wall $t)
                utcOffsetSeconds  = [int][TimeZoneInfo]::Local.GetUtcOffset($t).TotalSeconds
                segmentSuffix     = ($local.ToString('yyyyMMdd_HHmmss') + (& $offsetText $t))
                logStamp          = $local.ToString('yyyy-MM-dd HH:mm:ss zzz')
            }
        }

        # Five minutes either side, a real 600 s: what the local reading gets wrong.
        $a = $change.AddMinutes(-5); $b = $change.AddMinutes(5)
        $script:HolocronTimeNow = $change.AddSeconds(4)
        $snapshot = [pscustomobject]@{ SupervisorPid = $PID; State = 'running'; UpdatedAt = (Format-CamTimestamp -Value $change.AddSeconds(-4)) }
        $heartbeat = Get-CamSupervisorState -Config $config -Snapshot $snapshot
        $localA = $a.ToLocalTime(); $localB = $b.ToLocalTime()
        $interval = [ordered]@{
            fromUtc                = (& $utcText $a)
            toUtc                  = (& $utcText $b)
            trueSeconds            = 600
            utcElapsedSeconds      = ($b - $a).TotalSeconds
            localNaiveElapsedSeconds = ($localB - $localA).TotalSeconds
            localNaiveErrorSeconds = (($localB - $localA).TotalSeconds - 600)
            dueAfter5sUsingUtc     = (Test-CamIntervalDue -Last $a -Seconds 5 -Now $b)
            dueAfter5sUsingLocalNaive = (Test-CamIntervalDue -Last $localA -Seconds 5 -Now $localB)
            formattedFrom          = (Format-CamTimestamp -Value $a)
            formattedTo            = (Format-CamTimestamp -Value $b)
            formattedElapsedSeconds = ((Get-CamDateTimeOrNull -Value (Format-CamTimestamp -Value $b)) - (Get-CamDateTimeOrNull -Value (Format-CamTimestamp -Value $a))).TotalSeconds
            heartbeatWrittenAtUtc  = (& $utcText $change.AddSeconds(-4))
            heartbeatReadAtUtc     = (& $utcText $change.AddSeconds(4))
            heartbeatUpdatedAt     = $snapshot.UpdatedAt
            heartbeatAgeSeconds    = $heartbeat.AgeSeconds
            heartbeatLimitSeconds  = $limit
            heartbeatFresh         = ($heartbeat.AgeSeconds -le $limit)
        }
        $blocks += [ordered]@{
            kind             = [string]$tr.Kind
            changeUtc        = (& $utcText $change)
            deltaSeconds     = [int]$tr.DeltaSeconds
            localBefore      = ((& $wall $change.AddSeconds(-1)) + ' ' + (& $offsetText $change.AddSeconds(-1)))
            localAfter       = ((& $wall $change) + ' ' + (& $offsetText $change))
            instants         = $instants
            interval         = $interval
        }
    }

    $doc = [ordered]@{
        fixture     = 'Generated by tests/holocron/regen-goldens.ps1 from the PowerShell product in the zone GTB Standard Time. No machine clock or zone was changed.'
        description = 'Instants (UTC) around the two transitions with the text Format-CamTimestamp writes for them, the instant Get-CamDateTimeOrNull reads back, the local wall clock, the offset, the segment-name suffix %Y%m%d_%H%M%S%z and the log stamp. interval: the same 600 real seconds measured on UTC (correct) and on local readings (wrong by the transition delta).'
        zone        = [ordered]@{ windows = [TimeZoneInfo]::Local.Id; iana = 'Europe/Bucharest' }
        timestampFormat = 'yyyy-MM-ddTHH:mm:ss.fffffff+hh:mm (7 fractional digits, numeric offset, never Z)'
        transitions = $blocks
    }
    Save-HolocronText -Path (Join-Path (Join-Path $OutDir 'time') 'dst.json') -Text (ConvertTo-HolocronJson -InputObject $doc)
}
