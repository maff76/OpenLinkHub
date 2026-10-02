"use strict";
$(document).ready(function () {
    window.i18n = {
        locale: null,
        values: {},

        setTranslations: function (locale, values) {
            this.locale = locale;
            this.values = values || {};
        },

        t: function (key, fallback = '') {
            return this.values[key] ?? fallback ?? key;
        }
    };

    $.ajax({
        url: '/api/language',
        method: 'GET',
        dataType: 'json',
        success: function (response) {
            if (response.status === 1 && response.data) {
                i18n.setTranslations(
                    response.data.code,
                    response.data.values
                );
            }
            loadDevices();
        },
        error: function () {
            console.error('Failed to load translations');
            loadDevices();
        }
    });

    let showLabels = false;

    function loadDevices() {
        const devicePlaceholder = $(".device-placeholder");
        devicePlaceholder.removeClass("ready").empty();

        const devicePlaceholder2 = $("#system-cards-add");
        devicePlaceholder2.removeClass("ready");

        $.ajax({
            url: '/api/dashboard/devices/get',
            type: 'GET',
            success: function (response) {
                if (response.status !== 1) return;

                const results = new Array(response.devices.length);
                let completed = 0;
                const total = response.devices.length;

                $.each(response.devices, function (index, value) {
                    $.ajax({
                        url: '/api/devices/' + value,
                        type: 'GET',
                        success: function (dev) {
                            if (dev.device) {
                                results[index] = renderDevice(dev);
                            }
                            completed++;

                            if (completed === total) {
                                devicePlaceholder.append(
                                    results.filter(Boolean).join("")
                                );
                                devicePlaceholder.addClass("ready");
                            }
                        }
                    });
                });
                devicePlaceholder2.addClass("ready");
            }
        });
    }

    function dashboardValue(value, fallback = "—") {
        if (value === undefined || value === null || value === "") {
            return fallback;
        }
        return value;
    }

    function renderStatusPill(label, value, cssClass = "") {
        if (value === undefined || value === null || value === "") {
            return "";
        }
        return `
            <span class="dashboard-status ${cssClass}">
                <span class="dashboard-status-label">${label}</span>
                <span class="dashboard-status-value">${value}</span>
            </span>
        `;
    }

    function renderCompactChannel(parent, device) {
        const label = showLabels && device?.label ? device.label : "";
        const displayName = label || device.name || parent.product || "Device";
        const hasTemperature = Number(device.temperature) > 0;
        const hasSpeed = device.HasSpeed === true || device.hasSpeed === true;
        const hasProfile = hasSpeed && typeof device.profile === "string" && device.profile.length > 0;
        const hasRgb = typeof device.rgb === "string" && device.rgb.length > 0;
        const serial = parent.serial;

        let primary = "";
        if (hasTemperature) {
            primary += `
                <div class="dashboard-reading temperature">
                    <span class="dashboard-reading-value" id="temp-${serial}-${device.channelId}">${device.temperatureString}</span>
                    <span class="dashboard-reading-label">${i18n.t('txtTemperature')}</span>
                </div>`;
        }
        if (hasSpeed) {
            primary += `
                <div class="dashboard-reading speed">
                    <span class="dashboard-reading-value" id="speed-${serial}-${device.channelId}">${device.rpm} RPM</span>
                    <span class="dashboard-reading-label">${i18n.t('txtSpeed')}</span>
                </div>`;
        }

        let status = "";
        if (hasProfile) {
            status += renderStatusPill("Speed profile", device.profile, "speed-profile");
        }
        if (hasRgb) {
            status += renderStatusPill("RGB", device.rgb, "rgb-profile");
        }

        // Keep less common telemetry visible without turning the overview into a control page.
        if (device.gpuTemperature > 0) {
            status += renderStatusPill(i18n.t('txtGpuLiquid'), device.gpuTemperatureString);
        }
        if (device.gpuRpm > 0) {
            status += renderStatusPill(i18n.t('txtGpuPump'), device.gpuRpm + " RPM");
        }
        if (device.speed > 0) {
            status += renderStatusPill("Clock", device.speed + " MHz");
        }
        if (device.size > 0) {
            status += renderStatusPill(i18n.t('txtMemorySize'), device.size + " GB");
        }

        return `
            <div class="dashboard-channel">
                <div class="dashboard-channel-name" title="${displayName}">${displayName}</div>
                <div class="dashboard-channel-readings">${primary}</div>
                ${status ? `<div class="dashboard-channel-status">${status}</div>` : ""}
            </div>
        `;
    }

    function renderDevice(dev) {
        const parent = dev.device;
        const parentLabel = showLabels && parent.DeviceProfile?.Label ? parent.DeviceProfile.Label : "";
        const sectionTitle = parentLabel || parent.product || parent.Product || "Device";

        // Keep the existing richer PSU presentation; its voltage/current rails do not fit the compact
        // temperature/RPM dashboard layout particularly well.
        if (parent.IsPSU) {
            let html = `<div class="row g-4 mb-4 align-items-start">`;
            $.each(parent.devices, function (_, device) {
                if (device.IsTemperatureProbe || device.HasSpeed || device.Output) {
                    return;
                }
                const label = showLabels && device?.label ? device.label : "";
                html += `
                    <div class="col-md-3">
                        <div class="card system-card">
                            <div class="card-header header-split">
                                <span class="header-left">${device.name}</span>
                                <span class="header-right">${label}</span>
                            </div>
                            <div class="card-body"><div class="settings-list">`;
                if (device.MainPSU) {
                    html += `
                        <div class="settings-row"><span class="settings-label text-ellipsis">${i18n.t('txtSpeed')}</span><span class="meta-value" id="speed-${parent.serial}-${device.channelId}">${device.rpm} RPM</span></div>
                        <div class="settings-row"><span class="settings-label text-ellipsis">${i18n.t('txtVrmTemperature')}</span><span class="meta-value" id="vrm-temp-${parent.serial}-${device.channelId}">${device.vrmTemperatureString}</span></div>
                        <div class="settings-row"><span class="settings-label text-ellipsis">${i18n.t('txtPsuTemperature')}</span><span class="meta-value" id="psu-temp-${parent.serial}-${device.channelId}">${device.psuTemperatureString}</span></div>`;
                }
                if (device.HasWatts) html += `<div class="settings-row"><span class="settings-label text-ellipsis">${i18n.t('txtWatts')}</span><span class="meta-value" id="watts-${parent.serial}-${device.channelId}">${device.watts} W</span></div>`;
                if (device.HasAmps) html += `<div class="settings-row"><span class="settings-label text-ellipsis">${i18n.t('txtAmps')}</span><span class="meta-value" id="amps-${parent.serial}-${device.channelId}">${device.amps} A</span></div>`;
                if (device.HasVolts) html += `<div class="settings-row"><span class="settings-label text-ellipsis">${i18n.t('txtVolts')}</span><span class="meta-value" id="volts-${parent.serial}-${device.channelId}">${device.volts} V</span></div>`;
                html += `</div></div></div></div>`;
            });
            html += `</div>`;
            return html;
        }

        // Single devices (CPU blocks, etc.) get the same read-only dashboard language.
        if (parent.devices === null) {
            let readings = "";
            if (parent.Temperature > 0 || parent.temperature > 0) {
                readings = `<div class="dashboard-reading temperature"><span class="dashboard-reading-value" id="temperature-0">${parent.temperatureString}</span><span class="dashboard-reading-label">${parent.AIO || parent.IsCpuBlock ? i18n.t('txtLiquidTemp') : i18n.t('txtTemperature')}</span></div>`;
            }
            return `
                <section class="dashboard-device-section">
                    <div class="dashboard-device-heading"><span>${sectionTitle}</span></div>
                    <div class="dashboard-single-device">${readings}</div>
                </section>`;
        }

        let channels = "";
        $.each(parent.devices, function (_, device) {
            const hasSpeed = device.HasSpeed === true || device.hasSpeed === true;
            const hasTemps = device.HasTemps === true || device.hasTemps === true || Number(device.temperature) > 0;
            if (!hasSpeed && !hasTemps) {
                return;
            }
            channels += renderCompactChannel(parent, device);
        });

        if (!channels) {
            return "";
        }

        return `
            <section class="dashboard-device-section">
                <div class="dashboard-device-heading">
                    <span>${sectionTitle}</span>
                    <span class="dashboard-device-count">${parent.devices.length} channels</span>
                </div>
                <div class="dashboard-channel-grid">${channels}</div>
            </section>
        `;
    }

    function autoRefresh() {
        setInterval(function(){
            $.ajax({
                url:'/api/devices/',
                type:'get',
                success:function(result){
                    $.each(result.devices, function( index, value ) {
                        const serialId = value.Serial
                        if (value.GetDevice != null) {
                            if (value.GetDevice.devices == null) {
                                // Single device, e.g CPU block
                                const elementTemperatureId = "#temperature-0";
                                $(elementTemperatureId).html(value.GetDevice.temperatureString);
                            } else {
                                $.each(value.GetDevice.devices, function( key, device ) {
                                    const elementSpeedId = "#speed-" + serialId + "-" + device.channelId;
                                    const elementTemperatureId = "#temp-" + serialId + "-" + device.channelId;
                                    const elementVrmTemperatureId = "#vrm-temp-" + serialId + "-" + device.channelId;
                                    const elementPsuTemperatureId = "#psu-temp-" + serialId + "-" + device.channelId;
                                    const elementWatts = "#watts-" + serialId + "-" + device.channelId;
                                    const elementAmps = "#amps-" + serialId + "-" + device.channelId;
                                    const elementVolts = "#volts-" + serialId + "-" + device.channelId;

                                    $(elementWatts).html(device.watts + " W");
                                    $(elementAmps).html(device.amps + " A");
                                    $(elementVolts).html(device.volts + " V");
                                    $(elementSpeedId).html(device.rpm + " RPM");

                                    $(elementTemperatureId).html(device.temperatureString);
                                    $(elementVrmTemperatureId).html(device.vrmTemperatureString);
                                    $(elementPsuTemperatureId).html(device.psuTemperatureString);

                                    if (device.IsPSU) {
                                        const elementPowerOut = "#powerOut-" + device.channelId;
                                        if (elementPowerOut != null) {
                                            $(elementPowerOut).html(device.powerOutString + " W");
                                        }
                                    }

                                    if (device.volts) {
                                        $.each(device.volts, function( index, value ) {
                                            const amps = device.amps[index];
                                            const watts = device.watts[index];

                                            const elementVolts = "#volts-" + device.channelId + "-" + index;
                                            if (elementVolts != null) {
                                                $(elementVolts).html(value.ValueString + " V");
                                            }

                                            const elementAmps = "#amps-" + device.channelId + "-" + index;
                                            if (elementAmps != null) {
                                                $(elementAmps).html(amps.ValueString + " A");
                                            }

                                            const elementWatts = "#watts-" + device.channelId + "-" + index;
                                            if (elementWatts != null) {
                                                $(elementWatts).html(watts.ValueString + " W");
                                            }
                                        });
                                    }
                                });
                            }
                        }
                    });
                }
            });
        },3000);
    }
    autoRefresh();

    $('.allDevicesRgb').on('change', function () {
        const profile = $(this).val();
        if (profile === "none") {
            return false;
        }
        
        const pf = {
            "profile": profile
        };

        const json = JSON.stringify(pf, null, 2);

        $.ajax({
            url: '/api/color/global',
            type: 'POST',
            data: json,
            cache: false,
            success: function(response) {
                try {
                    if (response.status === 1) {
                        toast.success(response.message);
                    } else {
                        toast.warning(response.message);
                    }
                } catch (err) {
                    toast.warning(response.message);
                }
            }
        });
    });

    $('.addDeviceToDashboard').on('click', function () {
        const deviceId = $("#dashboardDeviceSelect").val();
        const pf = {};
        pf["deviceId"] = deviceId;
        const json = JSON.stringify(pf, null, 2);

        $.ajax({
            url: '/api/dashboard/devices/add',
            type: 'POST',
            data: json,
            cache: false,
            success: function(response) {
                try {
                    if (response.status === 1) {
                        loadDevices()
                    } else {
                        toast.warning(response.message);
                    }
                } catch (err) {
                    toast.warning(response.message);
                }
            }
        });
    });

    $('.deleteDeviceFromDashboard').on('click', function () {
        const deviceId = $("#dashboardDeviceSelect").val();
        const pf = {};
        pf["deviceId"] = deviceId;
        const json = JSON.stringify(pf, null, 2);

        $.ajax({
            url: '/api/dashboard/devices/delete',
            type: 'DELETE',
            data: json,
            cache: false,
            success: function(response) {
                try {
                    if (response.status === 1) {
                        loadDevices()
                    } else {
                        toast.warning(response.message);
                    }
                } catch (err) {
                    toast.warning(response.message);
                }
            }
        });
    });

    function loadDashboardSettings() {
        // Load current settings
        $.ajax({
            url: '/api/dashboard',
            type: 'GET',
            cache: false,
            success: function (response) {
                if (response.status === 1) {
                    showLabels = response.dashboard.showLabels;
                    if (response.dashboard.addDeviceToDashboard !== true) {
                        $("#system-cards-add").hide();
                    }
                }
            }
        });
    }

    loadDashboardSettings();
});