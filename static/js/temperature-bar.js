$(document).ready(function () {
    function getTemperatures() {
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
