$(document).ready(function () {
    // Keep useful product/model names on the overview without the long
    // storage identifier suffix. This is display-only; the API data is unchanged.
    $(".temperature-storage-item .temperature-compact-name").each(function () {
        const fullName = $(this).text().trim();
        let displayName = fullName;

        if (/^Seagate\s+FireCuda\s+530\b/i.test(fullName)) {
            displayName = "Seagate FireCuda 530";
        }

        if (displayName !== fullName) {
            $(this).text(displayName);
            $(this).attr("title", fullName);
        }
    });

    function memoryValue(value) {
        return value !== undefined && value !== null && value !== "" ? value : null;
    }

    function memoryDisplayName(device) {
        const label = typeof device.label === "string" ? device.label.trim() : "";
        if (label && label.toLowerCase() !== "set label") return label;
        const name = device.name || device.partNumber || "Memory";
        return Number(device.channelId) > 0 ? `${name} · Ch ${device.channelId}` : name;
    }

    function renderMemorySummary(devices) {
        const group = $("#dashboard-memory-group");
        const grid = $("#dashboard-memory-grid");
        if (!group.length || !grid.length) return;

        const dimms = [];
        $.each(devices || [], function (_, entry) {
            const parent = entry.GetDevice;
            if (!parent || !parent.devices) return;
            $.each(parent.devices, function (_, device) {
                if (Number(device.memoryType) === 4 || Number(device.memoryType) === 5 || device.xmpPresent === true || device.jedecDataRate > 0) {
                    dimms.push(device);
                }
            });
        });

        if (!dimms.length) {
            group.hide();
            grid.empty();
            return;
        }

        dimms.sort((a, b) => Number(a.channelId) - Number(b.channelId));
        let html = "";
        $.each(dimms, function (_, device) {
            const type = Number(device.memoryType) > 0 ? `DDR${device.memoryType}` : "Memory";
            const configured = Number(device.configuredSpeed) > 0 ? `${type}-${device.configuredSpeed}` : null;
            const jedec = Number(device.jedecDataRate) > 0 ? `${type}-${device.jedecDataRate}` : null;
            const xmpRate = Number(device.xmpDataRate) > 0 ? `${type}-${device.xmpDataRate}` : null;
            const xmpTiming = Number(device.xmpCasLatency) > 0 && Number(device.xmpTRAS) > 0
                ? `${device.xmpCasLatency}-${device.xmpTRCD}-${device.xmpTRP}-${device.xmpTRAS}` : null;
            const voltage = Number(device.xmpVdd) > 0 ? `${Number(device.xmpVdd).toFixed(3)} V` : null;
            const capacity = memoryValue(device.capacity);
            const temp = memoryValue(device.temperatureString) || (Number(device.temperature) > 0 ? `${Number(device.temperature).toFixed(1)} °C` : "—");
            const address = memoryValue(device.i2cAddress);

            const details = [];
            if (configured) details.push(`<span><strong>Configured</strong> ${configured}</span>`);
            else if (jedec) details.push(`<span><strong>JEDEC</strong> ${jedec}</span>`);
            if (capacity) details.push(`<span><strong>Capacity</strong> ${capacity}</span>`);
            if (xmpRate) details.push(`<span><strong>XMP</strong> ${xmpRate}${xmpTiming ? ` · ${xmpTiming}` : ""}${voltage ? ` · ${voltage}` : ""}</span>`);
            if (address) details.push(`<span><strong>SPD</strong> ${address}</span>`);

            const subtitle = device.partNumber || device.manufacturer || `${type} DIMM`;
            html += `
                <div class="temperature-compact-item temperature-memory-item">
                    <div class="temperature-compact-value">${temp}</div>
                    <div class="temperature-compact-name" title="${memoryDisplayName(device)}">${memoryDisplayName(device)}</div>
                    <div class="temperature-compact-detail" title="${subtitle}">${subtitle}</div>
                    <div class="temperature-memory-meta">${details.join("")}</div>
                </div>`;
        });
        grid.html(html);
        group.show();
    }

    function getTemperatures() {
        $.ajax({
            url:'/api/devices/',
            type:'get',
            success:function(result){
                renderMemorySummary(result.devices || []);
            }
        });
        $.ajax({
            url:'/api/cpuTemp',
            type:'get',
            success:function(result){
                $("#cpu_temp").html(result.data);
            }
        });
        $.ajax({
            url:'/api/gpuTemps',
            type:'get',
            success:function(result){
                $.each(result.data, function(index, value) {
                    $("#gpu_temp_" + index).html(value);
                });
            }
        });
        $.ajax({
            url:'/api/storageTemp',
            type:'get',
            success:function(result){
                $.each(result.data, function(index, value) {
                    $("#storage_temp-" + value.Key).html(value.TemperatureString);
                });
            }
        });
        $.ajax({
            url:'/api/hwmonTemps',
            type:'get',
            success:function(result){
                $.each(result.data || [], function(index, value) {
                    const id = "#hwmon_temp-" + value.HwmonName + "-" + value.InputName;
                    $(id).html(Number(value.TempC).toFixed(1) + " °C");
                });
            }
        });
    }

    function autoRefresh() {
        getTemperatures();
        setInterval(function(){
            getTemperatures();
        },3000);
    }
    autoRefresh();
});
